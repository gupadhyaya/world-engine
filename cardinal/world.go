package cardinal

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"time"

	"github.com/rotisserie/eris"
	"pkg.world.dev/world-engine/cardinal/ecs/storage/redis"
	"pkg.world.dev/world-engine/cardinal/gamestage"
	"pkg.world.dev/world-engine/cardinal/types/message"

	"github.com/rs/zerolog/log"
	"pkg.world.dev/world-engine/cardinal/ecs"
	"pkg.world.dev/world-engine/cardinal/ecs/ecb"
	"pkg.world.dev/world-engine/cardinal/ecs/receipt"
	"pkg.world.dev/world-engine/cardinal/events"
	"pkg.world.dev/world-engine/cardinal/evm"
	"pkg.world.dev/world-engine/cardinal/server"
	"pkg.world.dev/world-engine/cardinal/types/component"
	"pkg.world.dev/world-engine/cardinal/types/entity"
)

var ErrEntitiesCreatedBeforeStartGame = errors.New("entities should not be created before start game")

type World struct {
	instance           *ecs.World
	server             *server.Handler
	evmServer          evm.Server
	gameManager        *server.GameManager
	tickChannel        <-chan time.Time
	tickDoneChannel    chan<- uint64
	serverOptions      []server.Option
	gameManagerOptions []server.GameManagerOptions
	cleanup            func()

	// gameSequenceStage describes what stage the game is in (e.g. starting, running, shut down, etc)
	gameSequenceStage gamestage.Atomic
	endStartGame      chan bool
}

type (
	// EntityID represents a single entity in the World. An EntityID is tied to
	// one or more components.
	EntityID = entity.ID
	TxHash   = message.TxHash
	Receipt  = receipt.Receipt

	// System is a function that process the transaction in the given transaction queue.
	// Systems are automatically called during a world tick, and they must be registered
	// with a world using RegisterSystems.
	System func(WorldContext) error
)

// NewWorld creates a new World object using Redis as the storage layer.
func NewWorld(opts ...WorldOption) (*World, error) {
	ecsOptions, serverOptions, cardinalOptions := separateOptions(opts)

	// Load config. Fallback value is used if it's not set.
	cfg := GetWorldConfig()

	// Sane default options
	serverOptions = append(serverOptions, server.WithCORS())
	gameManagerOptions := []server.GameManagerOptions{} // not exposed in NewWorld Yet

	if cfg.CardinalMode == ModeProd {
		log.Logger.Info().Msg("Starting a new Cardinal world in production mode")
		if cfg.RedisPassword == DefaultRedisPassword {
			return nil, errors.New("redis password is required in production")
		}
		if cfg.CardinalNamespace == DefaultNamespace {
			return nil, errors.New(
				"cardinal namespace can't be the default value in production to avoid replay attack",
			)
		}
	} else {
		log.Logger.Info().Msg("Starting a new Cardinal world in development mode")
		ecsOptions = append(ecsOptions, ecs.WithPrettyLog())
		serverOptions = append(serverOptions, server.WithPrettyPrint())
		gameManagerOptions = append(gameManagerOptions, server.WithGameManagerPrettyPrint)
	}
	redisStore := redis.NewRedisStorage(redis.Options{
		Addr:     cfg.RedisAddress,
		Password: cfg.RedisPassword,
		DB:       0, // use default DB
	}, cfg.CardinalNamespace)
	storeManager, err := ecb.NewManager(redisStore.Client)
	if err != nil {
		return nil, err
	}

	ecsWorld, err := ecs.NewWorld(
		&redisStore,
		storeManager,
		ecs.Namespace(cfg.CardinalNamespace),
		ecsOptions...,
	)
	if err != nil {
		return nil, err
	}

	world := &World{
		instance:           ecsWorld,
		serverOptions:      serverOptions,
		gameManagerOptions: gameManagerOptions,
		endStartGame:       make(chan bool),
		gameSequenceStage:  gamestage.NewAtomic(),
	}

	// Apply options
	for _, opt := range cardinalOptions {
		opt(world)
	}

	return world, nil
}

// NewMockWorld creates a World object that uses miniredis as the storage layer suitable for local development.
// If you are creating a World for unit tests, use NewTestWorld.
func NewMockWorld(opts ...WorldOption) (*World, error) {
	world, err := NewWorld(append(opts, withMockRedis())...)
	if err != nil {
		return world, err
	}
	return world, nil
}

// CreateMany creates multiple entities in the world, and returns the slice of ids for the newly created
// entities. At least 1 component must be provided.
func CreateMany(wCtx WorldContext, num int, components ...component.Component) ([]EntityID, error) {
	return ecs.CreateMany(wCtx.Instance(), num, components...)
}

// Create creates a single entity in the world, and returns the id of the newly created entity.
// At least 1 component must be provided.
func Create(wCtx WorldContext, components ...component.Component) (EntityID, error) {
	return ecs.Create(wCtx.Instance(), components...)
}

// SetComponent Set sets component data to the entity.
func SetComponent[T component.Component](wCtx WorldContext, id entity.ID, comp *T) error {
	return ecs.SetComponent[T](wCtx.Instance(), id, comp)
}

// GetComponent Get returns component data from the entity.
func GetComponent[T component.Component](wCtx WorldContext, id entity.ID) (*T, error) {
	return ecs.GetComponent[T](wCtx.Instance(), id)
}

// UpdateComponent Updates a component on an entity.
func UpdateComponent[T component.Component](wCtx WorldContext, id entity.ID, fn func(*T) *T) error {
	return ecs.UpdateComponent[T](wCtx.Instance(), id, fn)
}

// AddComponentTo Adds a component on an entity.
func AddComponentTo[T component.Component](wCtx WorldContext, id entity.ID) error {
	return ecs.AddComponentTo[T](wCtx.Instance(), id)
}

// RemoveComponentFrom Removes a component from an entity.
func RemoveComponentFrom[T component.Component](wCtx WorldContext, id entity.ID) error {
	return ecs.RemoveComponentFrom[T](wCtx.Instance(), id)
}

// Remove removes the given entity id from the world.
func Remove(wCtx WorldContext, id EntityID) error {
	return wCtx.Instance().GetWorld().Remove(id)
}

func (w *World) handleShutdown() {
	signalChannel := make(chan os.Signal, 1)
	go func() {
		signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
		for sig := range signalChannel {
			if sig == syscall.SIGINT || sig == syscall.SIGTERM {
				err := w.ShutDown()
				if err != nil {
					log.Err(err).Msgf("There was an error during shutdown.")
				}
				return
			}
		}
	}()
}

// StartGame starts running the world game loop. Each time a message arrives on the tickChannel, a world tick is
// attempted. In addition, an HTTP server (listening on the given port) is created so that game messages can be sent
// to this world. After StartGame is called, RegisterComponent, RegisterMessages, RegisterQueries, and RegisterSystems
// may not be called. If StartGame doesn't encounter any errors, it will block forever, running the server and ticking
// the game in the background.
func (w *World) StartGame() error {
	ok := w.gameSequenceStage.CompareAndSwap(gamestage.StagePreStart, gamestage.StageStarting)
	if !ok {
		return errors.New("game has already been started")
	}

	if err := w.instance.LoadGameState(); err != nil {
		if errors.Is(err, ecs.ErrEntitiesCreatedBeforeLoadingGameState) {
			return eris.Wrap(ErrEntitiesCreatedBeforeStartGame, "")
		}
		return err
	}
	if !w.instance.DoesWorldHaveAnEventHub() {
		w.instance.SetEventHub(events.CreateWebSocketEventHub())
	}
	eventHub := w.instance.GetEventHub()
	eventBuilder := events.CreateNewWebSocketBuilder("/events", events.CreateWebSocketEventHandler(eventHub))
	handler, err := server.NewHandler(w.instance, eventBuilder, w.serverOptions...)
	if err != nil {
		return err
	}
	w.server = handler

	w.evmServer, err = evm.NewServer(w.instance)
	if err != nil {
		if !errors.Is(eris.Cause(err), evm.ErrNoEVMTypes) {
			return err
		}
		w.instance.Logger.Debug().
			Msgf("no EVM messages or queries specified. EVM server will not run: %s", eris.ToString(err, true))
	} else {
		w.instance.Logger.Debug().Msg("running world with EVM server")
		err = w.evmServer.Serve()
		if err != nil {
			return err
		}
	}

	if w.tickChannel == nil {
		w.tickChannel = time.Tick(time.Second) //nolint:staticcheck // its ok.
	}
	w.instance.StartGameLoop(context.Background(), w.tickChannel, w.tickDoneChannel)
	gameManager := server.NewGameManager(w.instance, w.server, w.gameManagerOptions...)
	w.gameManager = &gameManager
	go func() {
		ok := w.gameSequenceStage.CompareAndSwap(gamestage.StageStarting, gamestage.StageRunning)
		if !ok {
			log.Fatal().Msg("game was started prematurely")
		}
		if err := w.server.Serve(); errors.Is(err, http.ErrServerClosed) {
			log.Info().Err(err).Msgf("the server has been closed: %s", eris.ToString(err, true))
		} else if err != nil {
			log.Fatal().Err(err).Msgf("the server has failed: %s", eris.ToString(err, true))
		}
	}()

	// handle shutdown via a signal
	w.handleShutdown()
	<-w.endStartGame
	return err
}

func (w *World) IsGameRunning() bool {
	return w.gameSequenceStage.Load() == gamestage.StageRunning
}

func (w *World) ShutDown() error {
	if w.cleanup != nil {
		w.cleanup()
	}
	ok := w.gameSequenceStage.CompareAndSwap(gamestage.StageRunning, gamestage.StageShuttingDown)
	if !ok {
		// Either the world hasn't been started, or we've already shut down.
		return nil
	}
	// The CompareAndSwap returned true, so this call is responsible for actually
	// shutting down the game.
	defer func() {
		w.gameSequenceStage.Store(gamestage.StageShutDown)
	}()
	if w.evmServer != nil {
		w.evmServer.Shutdown()
	}
	close(w.endStartGame)
	err := w.gameManager.Shutdown()
	if err != nil {
		return err
	}
	return nil
}

func RegisterSystems(w *World, systems ...System) error {
	for _, system := range systems {
		functionName := filepath.Base(runtime.FuncForPC(reflect.ValueOf(system).Pointer()).Name())
		sys := system
		w.instance.RegisterSystemWithName(
			func(wCtx ecs.WorldContext) error {
				return sys(
					&worldContext{
						instance: wCtx,
					},
				)
			}, functionName,
		)
	}
	return nil
}

func RegisterComponent[T component.Component](world *World) error {
	return ecs.RegisterComponent[T](world.instance)
}

// RegisterMessages adds the given messages to the game world. HTTP endpoints to queue up/execute these
// messages will automatically be created when StartGame is called. This Register method must only be called once.
func RegisterMessages(w *World, msgs ...AnyMessage) error {
	return w.instance.RegisterMessages(toMessageType(msgs)...)
}

// RegisterQuery adds the given query to the game world. HTTP endpoints to use these queries
// will automatically be created when StartGame is called. This function does not add EVM support to the query.
func RegisterQuery[Request any, Reply any](
	world *World,
	name string,
	handler func(wCtx WorldContext, req *Request) (*Reply, error),
) error {
	err := ecs.RegisterQuery[Request, Reply](
		world.instance,
		name,
		func(wCtx ecs.WorldContext, req *Request) (*Reply, error) {
			return handler(&worldContext{instance: wCtx}, req)
		},
	)
	if err != nil {
		return err
	}
	return nil
}

// RegisterQueryWithEVMSupport adds the given query to the game world. HTTP endpoints to use these queries
// will automatically be created when StartGame is called. This Register method must only be called once.
// This function also adds EVM support to the query.
func RegisterQueryWithEVMSupport[Request any, Reply any](
	world *World,
	name string,
	handler func(wCtx WorldContext, req *Request) (*Reply, error),
) error {
	err := ecs.RegisterQuery[Request, Reply](
		world.instance,
		name,
		func(wCtx ecs.WorldContext, req *Request) (*Reply, error) {
			return handler(&worldContext{instance: wCtx}, req)
		},
		ecs.WithQueryEVMSupport[Request, Reply],
	)
	if err != nil {
		return err
	}
	return nil
}

func (w *World) Instance() *ecs.World {
	return w.instance
}

func (w *World) CurrentTick() uint64 {
	return w.instance.CurrentTick()
}

func (w *World) Tick(ctx context.Context) error {
	return w.instance.Tick(ctx)
}

// Init Registers a system that only runs once on a new game before tick 0.
func (w *World) Init(system System) {
	w.instance.AddInitSystem(
		func(ecsWctx ecs.WorldContext) error {
			return system(&worldContext{instance: ecsWctx})
		},
	)
}
