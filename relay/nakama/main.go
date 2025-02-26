package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/rotisserie/eris"
	"pkg.world.dev/world-engine/sign"
)

const (
	OK = iota
	Cancelled
	Unknown
	InvalidArgument
	DeadlineExceeded
	NotFound
	AlreadyExists
	PermissionDenied
	ResourceExhausted
	FailedPrecondition
	Aborted
	OutOfRange
	Unimplemented
	Internal
	Unavailable
	DataLoss
	Unauthenticated
)

type receiptChan chan *Receipt

const (
	EnvCardinalAddr      = "CARDINAL_ADDR"
	EnvCardinalNamespace = "CARDINAL_NAMESPACE"

	cardinalCollection = "cardinalCollection"
	personaTagKey      = "personaTag"

	transactionEndpointPrefix = "/tx"
)

func getDebugModeFromEnvironment() bool {
	devModeString := os.Getenv("ENABLE_DEBUG")
	switch strings.ToLower(devModeString) {
	case "true":
		return true
	case "false":
		return false
	default:
		return false
	}
}

var (
	DebugEnabled                    = false
	ErrPersonaTagStorageObjNotFound = errors.New("persona tag storage object not found")
	ErrNoPersonaTagForUser          = errors.New("user does not have a verified persona tag")

	globalNamespace string

	globalPersonaTagAssignment = sync.Map{}

	globalReceiptsDispatcher *receiptsDispatcher
)

func InitModule(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	initializer runtime.Initializer,
) error {
	DebugEnabled = getDebugModeFromEnvironment()

	if err := initCardinalAddress(); err != nil {
		return eris.Wrap(err, "failed to init cardinal address")
	}

	if err := initNamespace(); err != nil {
		return eris.Wrap(err, "failed to init namespace")
	}

	initReceiptDispatcher(logger)

	if err := initEventHub(ctx, logger, nk); err != nil {
		return eris.Wrap(err, "failed to init event hub")
	}

	if err := initReceiptMatch(ctx, logger, db, nk, initializer); err != nil {
		return eris.Wrap(err, "unable to init match for receipt streaming")
	}

	notifier := newReceiptNotifier(logger, nk)

	if err := initPrivateKey(ctx, logger, nk); err != nil {
		return eris.Wrap(err, "failed to init private key")
	}

	if err := initPersonaTagAssignmentMap(ctx, logger, nk); err != nil {
		return eris.Wrap(err, "failed to init persona tag assignment map")
	}

	ptv := initPersonaTagVerifier(logger, nk, globalReceiptsDispatcher)

	if err := initPersonaTagEndpoints(logger, initializer, ptv, notifier); err != nil {
		return eris.Wrap(err, "failed to init persona tag endpoints")
	}

	if err := initCardinalEndpoints(logger, initializer, notifier); err != nil {
		return eris.Wrap(err, "failed to init cardinal endpoints")
	}

	if err := initAllowlist(logger, initializer); err != nil {
		return eris.Wrap(err, "failed to init allowlist endpoints")
	}

	if err := initSaveFileStorage(logger, initializer); err != nil {
		return eris.Wrap(err, "failed to init save file storage endpoint")
	}

	if err := initSaveFileQuery(logger, initializer); err != nil {
		return eris.Wrap(err, "failed to init save file query endpoint")
	}

	return nil
}

func initNamespace() error {
	globalNamespace = os.Getenv(EnvCardinalNamespace)
	if globalNamespace == "" {
		return eris.Errorf("must specify a cardinal namespace via %s", EnvCardinalNamespace)
	}
	return nil
}

func initReceiptDispatcher(log runtime.Logger) {
	globalReceiptsDispatcher = newReceiptsDispatcher()
	go globalReceiptsDispatcher.pollReceipts(log)
	go globalReceiptsDispatcher.dispatch(log)
}

func initEventHub(ctx context.Context, log runtime.Logger, nk runtime.NakamaModule) error {
	eventHub, err := createEventHub(log)
	if err != nil {
		return err
	}
	go func() {
		err := eventHub.Dispatch(log)
		if err != nil {
			log.Error("error initializing eventHub: %s", eris.ToString(err, true))
		}
	}()

	// for now send to everybody via notifications.
	go func() {
		channel := eventHub.Subscribe("main")
		for event := range channel {
			err := eris.Wrap(nk.NotificationSendAll(ctx, "event", map[string]interface{}{"message": event.message}, 1, true), "")
			if err != nil {
				log.Error("error sending notifications: %s", eris.ToString(err, true))
			}
		}
	}()

	return nil
}

func initReceiptMatch(ctx context.Context, logger runtime.Logger, _ *sql.DB, nk runtime.NakamaModule,
	initializer runtime.Initializer) error {
	err := eris.Wrap(initializer.RegisterMatch("lobby", func(ctx context.Context, logger runtime.Logger, db *sql.DB,
		nk runtime.NakamaModule) (runtime.Match, error) {
		return &ReceiptMatch{}, nil
	}), "")
	if err != nil {
		logger.Error("unable to register match: %s", eris.ToString(err, true))
		return err
	}
	result, err := nk.MatchCreate(ctx, "lobby", map[string]any{})
	err = eris.Wrap(err, "")
	if err != nil {
		logger.Error("unable to create match: %s", eris.ToString(err, true))
		return err
	}
	logger.Debug("match create result is %q", result)
	return nil
}

// initPersonaTagAssignmentMap initializes a sync.Map with all the existing mappings of PersonaTag->UserID. This
// sync.Map ensures that multiple users will not be given the same persona tag.
func initPersonaTagAssignmentMap(ctx context.Context, logger runtime.Logger, nk runtime.NakamaModule) error {
	logger.Debug("attempting to build personaTag->userID mapping")
	var cursor string
	var objs []*api.StorageObject
	var err error
	iterationLimit := 100
	for {
		objs, cursor, err = nk.StorageList(ctx, "", cardinalCollection, iterationLimit, cursor)
		if err != nil {
			return eris.Wrap(err, "")
		}
		logger.Debug("found %d persona tag storage objects", len(objs))
		for _, obj := range objs {
			userID := obj.UserId
			var ptr *personaTagStorageObj
			ptr, err = storageObjToPersonaTagStorageObj(obj)
			if err != nil {
				return err
			}
			if ptr.Status == personaTagStatusAccepted || ptr.Status == personaTagStatusPending {
				logger.Debug("%s has been assigned to %s", ptr.PersonaTag, userID)
				globalPersonaTagAssignment.Store(ptr.PersonaTag, userID)
			}
		}
		if cursor == "" {
			break
		}
	}
	return nil
}

// initPersonaEndpoints sets up the nakame RPC endpoints that are used to claim a persona tag and display a persona tag.
func initPersonaTagEndpoints(
	_ runtime.Logger,
	initializer runtime.Initializer,
	ptv *personaTagVerifier,
	notifier *receiptNotifier) error {
	if err := initializer.RegisterRpc("nakama/claim-persona", handleClaimPersona(ptv, notifier)); err != nil {
		return eris.Wrap(err, "")
	}
	return eris.Wrap(initializer.RegisterRpc("nakama/show-persona", handleShowPersona), "")
}

// getUserID gets the Nakama UserID from the given context.
func getUserID(ctx context.Context) (string, error) {
	userID, ok := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if !ok {
		return "", eris.New("unable to get user id from context")
	}
	return userID, nil
}

// nakamaRPCHandler is the signature required for handlers that are passed to Nakama's RegisterRpc method.
// This type is defined just to make the function below a little more readable.
type nakamaRPCHandler func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule,
	payload string) (string, error)

// handleClaimPersona handles a request to Nakama to associate the current user with the persona tag in the payload.
//
//nolint:gocognit
func handleClaimPersona(ptv *personaTagVerifier, notifier *receiptNotifier) nakamaRPCHandler {
	return func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (
		string, error) {
		userID, err := getUserID(ctx)
		if err != nil {
			return logErrorMessageFailedPrecondition(logger, err, "unable to get userID")
		}

		// check if the user is verified. this requires them to input a valid beta key.
		err = checkVerified(ctx, nk, userID)
		if err != nil {
			if eris.Is(eris.Cause(err), ErrNotAllowlisted) {
				return logDebugWithMessageAndCode(logger, err, AlreadyExists, "unable to claim persona tag")
			}
			return logErrorMessageFailedPrecondition(logger, err, "unable to claim persona tag")
		}

		ptr := &personaTagStorageObj{}
		if err := json.Unmarshal([]byte(payload), ptr); err != nil {
			return logErrorMessageFailedPrecondition(logger, eris.Wrap(err, ""), "unable to marshal payload")
		}
		if ptr.PersonaTag == "" {
			return logErrorWithMessageAndCode(
				logger,
				eris.New("personaTag field was empty"),
				InvalidArgument,
				"personaTag field must not be empty",
			)
		}

		tag, err := loadPersonaTagStorageObj(ctx, nk)
		if err != nil {
			if !errors.Is(err, ErrPersonaTagStorageObjNotFound) {
				return logErrorMessageFailedPrecondition(logger, err, "unable to get persona tag storage object")
			}
		} else {
			switch tag.Status {
			case personaTagStatusPending:
				return logDebugWithMessageAndCode(
					logger,
					eris.Errorf("persona tag %q is pending for this account", tag.PersonaTag),
					AlreadyExists,
					"persona tag %q is pending", tag.PersonaTag,
				)
			case personaTagStatusAccepted:
				return logErrorWithMessageAndCode(
					logger,
					eris.Errorf("persona tag %q already associated with this account", tag.PersonaTag),
					AlreadyExists,
					"persona tag %q already associated with this account",
					tag.PersonaTag)
			case personaTagStatusRejected:
				// if the tag was rejected, don't do anything. let the user try to claim another tag.
			}
		}

		txHash, tick, err := cardinalCreatePersona(ctx, nk, ptr.PersonaTag)
		if err != nil {
			return logErrorMessageFailedPrecondition(logger, err, "unable to make create persona request to cardinal")
		}
		notifier.AddTxHashToPendingNotifications(txHash, userID)

		ptr.Status = personaTagStatusPending
		if err = ptr.savePersonaTagStorageObj(ctx, nk); err != nil {
			return logErrorMessageFailedPrecondition(logger, err, "unable to set persona tag storage object")
		}

		// Try to actually assign this personaTag->UserID in the sync map. If this succeeds, Nakama is OK with this
		// user having the persona tag.
		if ok := setPersonaTagAssignment(ptr.PersonaTag, userID); !ok {
			ptr.Status = personaTagStatusRejected
			if err = ptr.savePersonaTagStorageObj(ctx, nk); err != nil {
				return logErrorMessageFailedPrecondition(logger, err, "unable to set persona tag storage object")
			}
			return logErrorWithMessageAndCode(
				logger,
				eris.Errorf("persona tag %q is not available", ptr.PersonaTag),
				AlreadyExists,
				"persona tag %q is not available",
				ptr.PersonaTag)
		}

		ptr.Tick = tick
		ptr.TxHash = txHash
		if err = ptr.savePersonaTagStorageObj(ctx, nk); err != nil {
			return logErrorMessageFailedPrecondition(logger, err, "unable to save persona tag storage object")
		}
		ptv.addPendingPersonaTag(userID, ptr.TxHash)
		res, err := ptr.toJSON()
		if err != nil {
			return logErrorMessageFailedPrecondition(logger, err, "unable to marshal response")
		}
		return res, nil
	}
}

func handleShowPersona(ctx context.Context, logger runtime.Logger, _ *sql.DB, nk runtime.NakamaModule, _ string,
) (string, error) {
	ptr, err := loadPersonaTagStorageObj(ctx, nk)
	if err != nil {
		if eris.Is(eris.Cause(err), ErrPersonaTagStorageObjNotFound) {
			return logErrorMessageFailedPrecondition(logger, err, "no persona tag found")
		}
		return logErrorMessageFailedPrecondition(logger, err, "unable to get persona tag storage object")
	}
	ptr, err = ptr.attemptToUpdatePending(ctx, nk)
	if err != nil {
		return logErrorMessageFailedPrecondition(logger, err, "unable to update pending state")
	}
	res, err := ptr.toJSON()
	if err != nil {
		return logErrorMessageFailedPrecondition(logger, err, "unable to marshal response")
	}
	return res, nil
}

// initCardinalEndpoints queries the cardinal server to find the list of existing endpoints, and attempts to
// set up RPC wrappers around each one.
//
//nolint:gocognit,funlen // its fine.
func initCardinalEndpoints(logger runtime.Logger, initializer runtime.Initializer, notify *receiptNotifier) error {
	txEndpoints, queryEndpoints, err := getCardinalEndpoints()
	if err != nil {
		return err
	}

	createTransaction := func(payload string, endpoint string, nk runtime.NakamaModule, ctx context.Context,
	) (io.Reader, error) {
		logger.Debug("The %s endpoint requires a signed payload", endpoint)
		var transaction io.Reader
		transaction, err = makeTransaction(ctx, nk, payload)
		if err != nil {
			return nil, err
		}
		return transaction, nil
	}

	createUnsignedTransaction := func(payload string, endpoint string, _ runtime.NakamaModule, _ context.Context,
	) (io.Reader, error) {
		payloadBytes := []byte(payload)
		formattedPayloadBuffer := bytes.NewBuffer([]byte{})
		if !json.Valid(payloadBytes) {
			return nil, eris.Errorf("data %q is not valid json", string(payloadBytes))
		}
		err = json.Compact(formattedPayloadBuffer, payloadBytes)
		if err != nil {
			return nil, eris.Wrap(err, "")
		}
		return formattedPayloadBuffer, nil
	}

	registerEndpoints := func(endpoints []string, createPayload func(string, string, runtime.NakamaModule,
		context.Context) (io.Reader, error)) error {
		for _, e := range endpoints {
			logger.Debug("registering: %v", e)
			currEndpoint := e
			if currEndpoint[0] == '/' {
				currEndpoint = currEndpoint[1:]
			}
			err = initializer.RegisterRpc(currEndpoint, func(ctx context.Context, logger runtime.Logger, db *sql.DB,
				nk runtime.NakamaModule, payload string) (string, error) {
				logger.Debug("Got request for %q", currEndpoint)
				var resultPayload io.Reader
				resultPayload, err = createPayload(payload, currEndpoint, nk, ctx)
				if err != nil {
					return logErrorMessageFailedPrecondition(logger, err, "unable to make payload")
				}

				req, err := http.NewRequestWithContext(ctx, http.MethodPost, makeHTTPURL(currEndpoint), resultPayload)
				req.Header.Set("Content-Type", "application/json")
				if err != nil {
					return logErrorMessageFailedPrecondition(logger, err, "request setup failed for endpoint %q", currEndpoint)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return logErrorMessageFailedPrecondition(logger, err, "request failed for endpoint %q", currEndpoint)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						return logErrorMessageFailedPrecondition(
							logger,
							eris.Wrap(err, "failed to read response body"),
							"bad status code: %s: %s", resp.Status, body,
						)
					}
					return logErrorMessageFailedPrecondition(
						logger,
						eris.Errorf("bad status code %d", resp.StatusCode),
						"bad status code: %s: %s", resp.Status, body,
					)
				}
				bz, err := io.ReadAll(resp.Body)
				if err != nil {
					return logErrorMessageFailedPrecondition(logger, err, "can't read body")
				}
				if strings.HasPrefix(currEndpoint, transactionEndpointPrefix) {
					var asTx txResponse

					if err = json.Unmarshal(bz, &asTx); err != nil {
						return logErrorMessageFailedPrecondition(logger, err, "can't decode body as tx response")
					}
					userID, err := getUserID(ctx)
					if err != nil {
						return logErrorMessageFailedPrecondition(logger, err, "unable to get user id")
					}
					notify.AddTxHashToPendingNotifications(asTx.TxHash, userID)
				}

				return string(bz), nil
			})
			if err != nil {
				return eris.Wrap(err, "")
			}
		}
		return nil
	}

	err = registerEndpoints(txEndpoints, createTransaction)
	if err != nil {
		return err
	}
	err = registerEndpoints(queryEndpoints, createUnsignedTransaction)
	if err != nil {
		return err
	}
	return nil
}

func logDebugWithMessageAndCode(
	logger runtime.Logger,
	err error,
	code int,
	format string,
	v ...interface{},
) (string, error) {
	err = eris.Wrapf(err, format, v...)
	return logDebug(logger, err, code)
}

func logErrorWithMessageAndCode(
	logger runtime.Logger,
	err error,
	code int,
	format string,
	v ...interface{},
) (string, error) {
	err = eris.Wrapf(err, format, v...)
	return logError(logger, err, code)
}

func logErrorMessageFailedPrecondition(
	logger runtime.Logger,
	err error,
	format string,
	v ...interface{}) (string, error) {
	err = eris.Wrapf(err, format, v...)
	return logErrorFailedPrecondition(logger, err)
}

func logDebug(
	logger runtime.Logger,
	err error,
	code int,
) (string, error) {
	logger.Debug(eris.ToString(err, true))
	return "", errToNakamaError(err, code)
}

func logError(
	logger runtime.Logger,
	err error,
	code int) (string, error) {
	logger.Error(eris.ToString(err, true))
	return "", errToNakamaError(err, code)
}

func logErrorFailedPrecondition(
	logger runtime.Logger,
	err error) (string, error) {
	return logError(logger, err, FailedPrecondition)
}

func errToNakamaError(err error, code int) error {
	if err != nil {
		if DebugEnabled {
			return runtime.NewError(eris.ToString(err, true), code)
		}
		return runtime.NewError(err.Error(), code)
	}
	return nil
}

// setPersonaTagAssignment attempts to associate a given persona tag with the given user ID, and returns
// true if the attempt was successful or false if it failed. This method is safe for concurrent access.
func setPersonaTagAssignment(personaTag, userID string) (ok bool) {
	val, loaded := globalPersonaTagAssignment.LoadOrStore(personaTag, userID)
	if !loaded {
		return true
	}
	gotUserID, _ := val.(string)
	return gotUserID == userID
}

func makeTransaction(ctx context.Context, nk runtime.NakamaModule, payload string) (io.Reader, error) {
	ptr, err := loadPersonaTagStorageObj(ctx, nk)
	if err != nil {
		return nil, err
	}
	ptr, err = ptr.attemptToUpdatePending(ctx, nk)
	if err != nil {
		return nil, err
	}

	if ptr.Status != personaTagStatusAccepted {
		return nil, eris.Wrap(ErrNoPersonaTagForUser, "")
	}
	personaTag := ptr.PersonaTag
	pk, nonce, err := getPrivateKeyAndANonce(ctx, nk)
	if err != nil {
		return nil, err
	}
	sp, err := sign.NewTransaction(pk, personaTag, globalNamespace, nonce, payload)
	if err != nil {
		return nil, err
	}
	buf, err := json.Marshal(sp)
	if err != nil {
		return nil, eris.Wrap(err, "")
	}
	return bytes.NewReader(buf), nil
}
