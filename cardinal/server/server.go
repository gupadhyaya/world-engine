package server

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-openapi/loads"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/runtime/middleware"
	"github.com/go-openapi/runtime/middleware/untyped"
	"github.com/mitchellh/mapstructure"
	"github.com/rotisserie/eris"
	"github.com/rs/cors"
	"github.com/rs/zerolog/log"
	"pkg.world.dev/world-engine/cardinal/ecs"
	"pkg.world.dev/world-engine/cardinal/shard"
)

// Handler is a type that contains endpoints for messages and queries in a given ecs world.
type Handler struct {
	w                      *ecs.World
	Mux                    *http.ServeMux
	server                 *http.Server
	disableSigVerification bool
	Port                   string
	withCORS               bool
	running                atomic.Bool
	shutdownMutex          sync.Mutex

	// plugins
	adapter shard.WriteAdapter
}

var (
	// ErrInvalidSignature is returned when a signature is incorrect in some way (e.g. namespace mismatch, nonce invalid,
	// the actual Verify fails). Other failures (e.g. Redis is down) should not wrap this error.
	ErrInvalidSignature = errors.New("invalid signature")
)

const (
	gameQueryPrefix = "/query/game/"
	gameTxPrefix    = "/tx/game/"

	readHeaderTimeout = 5 * time.Second
)

// NewHandler instantiates handler function for creating a swagger server that validates itself based on a swagger spec.
// messages and queries registered with the given world are automatically created. The server runs on a default port
// of 4040, but can be changed via options or by setting an environment variable with key CARDINAL_PORT.
func NewHandler(w *ecs.World, builder middleware.Builder, opts ...Option) (*Handler, error) {
	h, err := newSwaggerHandlerEmbed(w, builder, opts...)
	h.running.Store(false)
	if err != nil {
		return nil, err
	}
	return h, nil
}

//go:embed swagger.yml
var swaggerData []byte

func newSwaggerHandlerEmbed(w *ecs.World, builder middleware.Builder, opts ...Option) (*Handler, error) {
	th := &Handler{
		w:        w,
		Mux:      http.NewServeMux(),
		withCORS: false,
	}
	for _, opt := range opts {
		opt(th)
	}
	specDoc, err := loads.Analyzed(swaggerData, "")
	if err != nil {
		return nil, eris.Wrap(err, "error loading swagger spec")
	}
	api := untyped.NewAPI(specDoc).WithoutJSONDefaults()
	api.RegisterConsumer("application/json", runtime.JSONConsumer())
	api.RegisterProducer("application/json", runtime.JSONProducer())
	err = th.registerTxHandlerSwagger(api)
	if err != nil {
		return nil, err
	}
	err = th.registerQueryHandlerSwagger(api)
	if err != nil {
		return nil, err
	}
	th.registerDebugHandlerSwagger(api)
	th.registerHealthHandlerSwagger(api)

	// This is here to meet the swagger spec. Actual /events will be intercepted before this route.
	api.RegisterOperation("GET", "/events", runtime.OperationHandlerFunc(func(params interface{}) (interface{}, error) {
		return struct{}{}, nil
	}))

	if err = api.Validate(); err != nil {
		return nil, eris.Wrap(err, "error validating api against spec")
	}

	app := middleware.NewContext(specDoc, api, nil)
	var handler = app.APIHandler(builder)
	if th.withCORS {
		handler = cors.AllowAll().Handler(handler)
	}
	th.Mux.Handle("/", handler)
	th.Initialize()

	return th, nil
}

// utility function to create a swagger handler from a request name, request constructor, request to response function.
func createSwaggerQueryHandler[Request any, Response any](requestName string,
	requestHandler func(*Request) (*Response, error)) runtime.OperationHandlerFunc {
	return func(params interface{}) (interface{}, error) {
		isEmpty, err := isParamsEmpty(params)
		if err != nil {
			return nil, err
		}
		var request *Request
		var ok bool
		if !isEmpty {
			request, ok = getValueFromParams[Request](params, requestName)
			if !ok {
				return middleware.Error(http.StatusNotFound, fmt.Errorf("%s not found", requestName)), nil
			}
		} else {
			request = nil
		}
		resp, err := requestHandler(request)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
}

func isParamsEmpty(params interface{}) (bool, error) {
	data, ok := params.(map[string]interface{})
	if !ok {
		return false, eris.New("params data structure must be a map[string]interface{}")
	}
	return len(data) == 0, nil
}

// getValueFromParams extracts parameters from swagger handlers.
func getValueFromParams[T any](params interface{}, name string) (*T, bool) {
	data, ok := params.(map[string]interface{})
	if !ok {
		return nil, ok
	}
	mappedStructUntyped, ok := data[name]
	if !ok {
		return nil, ok
	}
	mappedStruct, ok := mappedStructUntyped.(map[string]interface{})
	if !ok {
		return nil, ok
	}
	value := new(T)
	err := mapstructure.Decode(mappedStruct, value)
	if err != nil {
		return nil, ok
	}
	return value, true
}

// EndpointsResult result struct for /query/http/endpoints.
type EndpointsResult struct {
	TxEndpoints    []string `json:"txEndpoints"`
	QueryEndpoints []string `json:"queryEndpoints"`
	DebugEndpoints []string `json:"debugEndpoints"`
}

func createAllEndpoints(world *ecs.World) (*EndpointsResult, error) {
	txs, err := world.ListMessages()
	if err != nil {
		return nil, err
	}
	txEndpoints := make([]string, 0, len(txs))
	for _, tx := range txs {
		if tx.Name() == ecs.CreatePersonaMsg.Name() {
			txEndpoints = append(txEndpoints, "/tx/persona/"+tx.Name())
		} else {
			txEndpoints = append(txEndpoints, gameTxPrefix+tx.Name())
		}
	}

	queries := world.ListQueries()
	queryEndpoints := make([]string, 0, len(queries))
	for _, query := range queries {
		queryEndpoints = append(queryEndpoints, gameQueryPrefix+query.Name())
	}
	queryEndpoints = append(queryEndpoints,
		"/query/http/endpoints",
		"/query/persona/signer",
		"/query/receipt/list",
		"/query/game/cql",
	)
	debugEndpoints := make([]string, 1)
	debugEndpoints[0] = "/debug/state"
	return &EndpointsResult{
		TxEndpoints:    txEndpoints,
		QueryEndpoints: queryEndpoints,
	}, nil
}

// Initialize initializes the server. It firsts checks for a port set on the handler via options.
// if no port is found, or a bad port was passed into the option, it falls back to an environment variable,
// CARDINAL_PORT. If not set, it falls back to a default port of 4040.
func (handler *Handler) Initialize() {
	if _, err := strconv.Atoi(handler.Port); err != nil || len(handler.Port) == 0 {
		envPort := os.Getenv("CARDINAL_PORT")
		if _, err = strconv.Atoi(envPort); err == nil {
			handler.Port = envPort
		} else {
			handler.Port = "4040"
		}
	}
	handler.server = &http.Server{
		Addr:              fmt.Sprintf(":%s", handler.Port),
		Handler:           handler.Mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// Serve serves the application, blocking the calling thread.
// Call this in a new go routine to prevent blocking.
func (handler *Handler) Serve() error {
	hostname, err := os.Hostname()
	if err != nil {
		return eris.Wrap(err, "error getting hostname")
	}
	log.Info().Msgf("serving cardinal at %s:%s", hostname, handler.Port)
	handler.running.Store(true)
	err = eris.Wrap(handler.server.ListenAndServe(), "error listening and serving")
	handler.running.Store(false)
	return err
}

func (handler *Handler) Close() error {
	err := eris.Wrap(handler.server.Close(), "error closing server")
	if err != nil {
		return err
	}
	return nil
}

func (handler *Handler) Shutdown() error {
	handler.shutdownMutex.Lock()
	defer handler.shutdownMutex.Unlock()
	displayLogs := false
	if handler.running.Load() {
		// handler.running tracks whether the server is running.
		// for safety allow shutdown to happen whenever this method is called
		// EVEN if running reads that it is not running.
		// However, only display the log message if expected running state is consistent.
		// meaning that shutdown is called on a server where running is true.
		displayLogs = true
	}

	if displayLogs {
		log.Info().Msg("Shutting down server.")
	}
	ctx := context.Background()
	err := eris.Wrap(handler.server.Shutdown(ctx), "error shutting down http server")
	if err != nil {
		return err
	}
	if displayLogs {
		log.Info().Msg("Server successfully shutdown.")
	}
	return nil
}
