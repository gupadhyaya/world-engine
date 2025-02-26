package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/rotisserie/eris"
)

/*
	REQUEST MESSAGES
*/

type SaveGameRequest struct {
	Data string `json:"data"`
}

type SaveGameResponse struct {
	Success bool `json:"success"`
}

type GetSaveReply struct {
	Data        string `json:"data"`
	Persona     string `json:"persona"`
	Allowlisted bool   `json:"allowlisted"`
}

const (
	gameSaveCollection = "game_saves"
)

func initSaveFileStorage(_ runtime.Logger, initializer runtime.Initializer) error {
	err := initializer.RegisterRpc(
		"nakama/save",
		handleSaveGame,
	)
	if err != nil {
		return eris.Wrap(err, "")
	}
	return nil
}

func handleSaveGame(ctx context.Context, logger runtime.Logger, _ *sql.DB, nk runtime.NakamaModule, payload string,
) (string, error) {
	userID, err := getUserID(ctx)
	if err != nil {
		return logErrorMessageFailedPrecondition(logger, eris.Wrap(err, ""), "failed to get user ID")
	}

	var msg SaveGameRequest
	err = json.Unmarshal([]byte(payload), &msg)
	if err != nil {
		return logError(
			logger,
			eris.Wrap(err, `error unmarshalling payload: expected form {"data": <string>}`),
			InvalidArgument)
	}
	// do not allow empty requests
	if msg.Data == "" {
		return logErrorFailedPrecondition(
			logger,
			eris.New("data cannot be empty"),
		)
	}

	err = writeSave(ctx, userID, payload, nk)
	if err != nil {
		return logErrorFailedPrecondition(
			logger,
			eris.Wrap(err, "failed to write game save to storage"),
		)
	}

	response, err := json.Marshal(SaveGameResponse{Success: true})
	if err != nil {
		return logErrorFailedPrecondition(logger, eris.Wrap(err, "failed to marshal response"))
	}

	return string(response), nil
}

func writeSave(ctx context.Context, userID string, save string, nk runtime.NakamaModule) error {
	write := &runtime.StorageWrite{
		Collection:      gameSaveCollection,
		Key:             userID,
		UserID:          userID,
		Value:           save,
		Version:         "",
		PermissionRead:  runtime.STORAGE_PERMISSION_OWNER_READ,
		PermissionWrite: runtime.STORAGE_PERMISSION_OWNER_WRITE,
	}
	_, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{write})
	return err
}

func initSaveFileQuery(_ runtime.Logger, initializer runtime.Initializer) error {
	err := initializer.RegisterRpc(
		"nakama/get-save",
		handleGetSaveGame,
	)
	if err != nil {
		return eris.Wrap(err, "")
	}
	return nil
}

func handleGetSaveGame(ctx context.Context, logger runtime.Logger, _ *sql.DB, nk runtime.NakamaModule, _ string,
) (string, error) {
	userID, err := getUserID(ctx)
	if err != nil {
		return logErrorMessageFailedPrecondition(logger, eris.Wrap(err, ""), "failed to get user ID")
	}

	var personaTag string
	// get the persona storage object.
	persona, err := loadPersonaTagStorageObj(ctx, nk)
	if err != nil {
		// we ignore the error where the tag is not found.
		// all other errors should be returned.
		if !eris.Is(eris.Cause(err), ErrPersonaTagStorageObjNotFound) {
			return logErrorFailedPrecondition(logger, eris.Wrap(err, "failed to get persona for save"))
		}
	} else {
		if persona.Status == personaTagStatusAccepted {
			personaTag = persona.PersonaTag
		}
	}

	// check if the user is allowlisted. NOTE: checkVerified will return nil in two cases:
	// case 1: if the allowlist is disabled (via ENABLE_ALLOWLIST env var).
	// case 2: the user is actually allowlisted.
	var verified bool
	err = checkVerified(ctx, nk, userID)
	if err != nil {
		// as long as the error isn't that they're not allowlisted, return the error.
		// we ignore the ErrNotAllowlisted, which will keep verified == false.
		if !eris.Is(eris.Cause(err), ErrNotAllowlisted) {
			return logErrorFailedPrecondition(logger, eris.Wrap(err, "could not read verification table"))
		}
	} else {
		// when err == nil, that means checkVerified passed, or that there is no allowlist enabled.
		// so we just set verified to true.
		verified = true
	}

	var dataStr string
	data, err := readSave(ctx, userID, nk)
	if err != nil {
		// if no save is found, we just wanna return the empty string. so catch all other errors but that one.
		if !eris.Is(eris.Cause(err), ErrNoSaveFound) {
			return logErrorFailedPrecondition(logger, eris.Wrap(err, "failed to read save data"))
		}
	} else {
		var dataMsg SaveGameRequest
		err := json.Unmarshal([]byte(data), &dataMsg)
		if err != nil {
			return logErrorFailedPrecondition(logger, eris.Wrap(err, "failed to unmarshall save"))
		}
		dataStr = dataMsg.Data
	}

	saveData := GetSaveReply{
		Data:        dataStr,
		Persona:     personaTag,
		Allowlisted: verified,
	}
	saveBz, err := json.Marshal(saveData)
	if err != nil {
		return logErrorFailedPrecondition(logger, eris.Wrap(err, "failed to marshal save file"))
	}
	return string(saveBz), nil
}

var ErrNoSaveFound = errors.New("no save found")

func readSave(ctx context.Context, userID string, nk runtime.NakamaModule) (string, error) {
	read := &runtime.StorageRead{
		Collection: gameSaveCollection,
		Key:        userID,
		UserID:     userID,
	}
	saves, err := nk.StorageRead(ctx, []*runtime.StorageRead{read})
	if err != nil {
		return "", err
	}
	if len(saves) == 0 {
		return "", eris.Wrapf(ErrNoSaveFound, "")
	}
	if len(saves) != 1 {
		return "", eris.Errorf("expected 1 save file, got %d", len(saves))
	}
	return saves[0].Value, nil
}
