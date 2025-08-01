// SPDX-FileCopyrightText: 2025 Mads R. Havmand <mads@v42.dk>
//
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	connectcors "connectrpc.com/cors"
	"connectrpc.com/grpcreflect"
	"github.com/MadsRC/trustedai"
	"github.com/MadsRC/trustedai/gen/proto/madsrc/trustedai/v1/trustedaiv1connect"
	"github.com/MadsRC/trustedai/internal/api/auth"
	cauth "github.com/MadsRC/trustedai/internal/api/controlplane/auth"
	"github.com/MadsRC/trustedai/internal/api/controlplane/services"
	"github.com/rs/cors"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// WithFrontendFS returns a [ControlPlaneOption] that uses the provided filesystem for frontend files.
func WithFrontendFS(frontendFS fs.FS) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.FrontendFS = frontendFS
	})
}

func registerServiceHandlers(
	mux *http.ServeMux,
	interceptors []connect.Interceptor,
	servicesToRegister map[string]any,
) {
	for path, handler := range servicesToRegister {
		mux.Handle(path, handler.(http.Handler))
	}
}

type ControlPlaneServer struct {
	options     *controlPlaneOptions
	mux         *http.ServeMux
	corsHandler *cors.Cors
}

// NewControlPlaneServer creates a new [ControlPlaneServer].
func NewControlPlaneServer(options ...ControlPlaneOption) (*ControlPlaneServer, error) {
	opts := defaultControlPlaneOptions
	for _, opt := range GlobalControlPlaneOptions {
		opt.apply(&opts)
	}
	for _, opt := range options {
		opt.apply(&opts)
	}

	// Create auth interceptor if not provided
	authInterceptor := opts.AuthInterceptor
	if authInterceptor == nil && opts.SessionStore != nil {
		authInterceptor = cauth.NewInterceptor(opts.SessionStore)
	}

	// Create IAM service handler
	iamServiceHandler, err := services.NewIam(
		services.WithUserRepository(opts.UserRepository),
		services.WithOrganizationRepository(opts.OrganizationRepository),
		services.WithTokenRepository(opts.TokenRepository),
	)
	if err != nil {
		return nil, err
	}

	// Create Model Management service handler
	modelManagementServiceHandler, err := services.NewModelManagement(
		services.WithCredentialRepository(opts.CredentialRepository),
		services.WithModelRepository(opts.ModelRepository),
	)
	if err != nil {
		return nil, err
	}

	// Create Usage Analytics service handler
	usageAnalyticsServiceHandler, err := services.NewUsageAnalytics(
		services.WithUsageAnalyticsUserRepository(opts.UserRepository),
		services.WithUsageRepository(opts.UsageRepository),
		services.WithBillingRepository(opts.BillingRepository),
		services.WithUsageAnalyticsLogger(opts.Logger.With("service", "usage_analytics")),
	)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	// Configure CORS early for use with individual handlers
	allowedMethods := append(connectcors.AllowedMethods(), http.MethodOptions)
	allowedHeaders := append(connectcors.AllowedHeaders(),
		"Connect-Protocol-Version",
		"Connect-Timeout-Ms",
		"X-User-Agent",
		"User-Agent",
		"Accept-Encoding",
	)
	corsHandler := cors.New(cors.Options{
		AllowedOrigins:     []string{"http://localhost:3000"},
		AllowedMethods:     allowedMethods,
		AllowedHeaders:     allowedHeaders,
		ExposedHeaders:     connectcors.ExposedHeaders(),
		AllowCredentials:   true,
		OptionsPassthrough: false, // Handle OPTIONS internally
		MaxAge:             7200,  // 2 hours in seconds
		Debug:              true,  // Enable debug logging
	})

	// Create handler with interceptors
	interceptors := []connect.Interceptor{}
	if authInterceptor != nil {
		interceptors = append(interceptors, authInterceptor)
	}
	if opts.TokenInterceptor != nil {
		interceptors = append(interceptors, opts.TokenInterceptor)
	}

	servicesToRegister := make(map[string]any)

	// IAM service
	iamPath, iamHandler := trustedaiv1connect.NewIAMServiceHandler(
		iamServiceHandler,
		connect.WithInterceptors(interceptors...),
	)
	servicesToRegister[iamPath] = iamHandler

	// Model Management service
	modelManagementPath, modelManagementHandler := trustedaiv1connect.NewModelManagementServiceHandler(
		modelManagementServiceHandler,
		connect.WithInterceptors(interceptors...),
	)
	servicesToRegister[modelManagementPath] = modelManagementHandler

	// Usage Analytics service
	usageAnalyticsPath, usageAnalyticsHandler := trustedaiv1connect.NewUsageAnalyticsServiceHandler(
		usageAnalyticsServiceHandler,
		connect.WithInterceptors(interceptors...),
	)
	servicesToRegister[usageAnalyticsPath] = usageAnalyticsHandler

	registerServiceHandlers(mux, interceptors, servicesToRegister)

	// Add gRPC reflection support
	reflector := grpcreflect.NewStaticReflector(
		trustedaiv1connect.IAMServiceName,
		trustedaiv1connect.ModelManagementServiceName,
		trustedaiv1connect.UsageAnalyticsServiceName,
	)
	reflectionV1Path, reflectionV1Handler := grpcreflect.NewHandlerV1(reflector)
	reflectionV1AlphaPath, reflectionV1AlphaHandler := grpcreflect.NewHandlerV1Alpha(reflector)
	mux.Handle(reflectionV1Path, reflectionV1Handler)
	mux.Handle(reflectionV1AlphaPath, reflectionV1AlphaHandler)

	// Add SSO callback handler if provided
	if opts.SsoHandler != nil {
		opts.Logger.Info("Registering SSO handler at /sso/")
		mux.Handle("/sso/", http.StripPrefix("/sso", opts.SsoHandler))
	} else {
		opts.Logger.Warn("No SSO handler provided - SSO routes will not be available")
	}

	// Serve static frontend files from embedded filesystem at root
	if opts.FrontendFS != nil {
		fileServer := http.FileServer(http.FS(opts.FrontendFS))
		mux.Handle("/", fileServer)
	} else {
		// Fallback to filesystem if no embedded FS provided
		fileServer := http.FileServer(http.Dir("frontend/dist/"))
		mux.Handle("/", fileServer)
	}

	return &ControlPlaneServer{
		options:     &opts,
		mux:         mux,
		corsHandler: corsHandler,
	}, nil
}

type controlPlaneOptions struct {
	Logger                 *slog.Logger
	UserRepository         trustedai.UserRepository
	OrganizationRepository trustedai.OrganizationRepository
	TokenRepository        trustedai.TokenRepository
	ProviderRepository     trustedai.ProviderRepository
	CredentialRepository   trustedai.CredentialRepository
	ModelRepository        trustedai.ModelRepository
	UsageRepository        trustedai.UsageRepository
	BillingRepository      trustedai.BillingRepository
	SsoHandler             http.Handler
	SessionStore           auth.SessionStore
	AuthInterceptor        *cauth.Interceptor
	TokenInterceptor       *cauth.TokenInterceptor
	FrontendFS             fs.FS
}

var defaultControlPlaneOptions = controlPlaneOptions{
	Logger: slog.Default(),
}

// GlobalControlPlaneOptions is a list of [ControlPlaneOption]s that are applied to all [ControlPlaneServer]s.
var GlobalControlPlaneOptions []ControlPlaneOption

// ControlPlaneOption is an option for configuring a [ControlPlaneServer].
type ControlPlaneOption interface {
	apply(*controlPlaneOptions)
}

// funcControlPlaneOption is a [ControlPlaneOption] that calls a function.
// It is used to wrap a function, so it satisfies the [ControlPlaneOption] interface.
type funcControlPlaneOption struct {
	f func(*controlPlaneOptions)
}

func (fdo *funcControlPlaneOption) apply(opts *controlPlaneOptions) {
	fdo.f(opts)
}

func newFuncControlPlaneOption(f func(*controlPlaneOptions)) *funcControlPlaneOption {
	return &funcControlPlaneOption{
		f: f,
	}
}

// WithControlPlaneLogger returns a [ControlPlaneOption] that uses the provided logger.
func WithControlPlaneLogger(logger *slog.Logger) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.Logger = logger
	})
}

// WithControlPlaneUserRepository returns a [ControlPlaneOption] that uses the provided UserRepository.
func WithControlPlaneUserRepository(repository trustedai.UserRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.UserRepository = repository
	})
}

// WithControlPlaneOrganizationRepository returns a [ControlPlaneOption] that uses the provided OrganizationRepository.
func WithControlPlaneOrganizationRepository(repository trustedai.OrganizationRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.OrganizationRepository = repository
	})
}

// WithControlPlaneTokenRepository returns a [ControlPlaneOption] that uses the provided TokenRepository.
func WithControlPlaneTokenRepository(repository trustedai.TokenRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.TokenRepository = repository
	})
}

// WithControlPlaneProviderRepository returns a [ControlPlaneOption] that uses the provided ProviderRepository.
func WithControlPlaneProviderRepository(repository trustedai.ProviderRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.ProviderRepository = repository
	})
}

// WithControlPlaneCredentialRepository returns a [ControlPlaneOption] that uses the provided CredentialRepository.
func WithControlPlaneCredentialRepository(repository trustedai.CredentialRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.CredentialRepository = repository
	})
}

// WithControlPlaneModelRepository returns a [ControlPlaneOption] that uses the provided ModelRepository.
func WithControlPlaneModelRepository(repository trustedai.ModelRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.ModelRepository = repository
	})
}

// WithControlPlaneUsageRepository returns a [ControlPlaneOption] that uses the provided UsageRepository.
func WithControlPlaneUsageRepository(repository trustedai.UsageRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.UsageRepository = repository
	})
}

// WithControlPlaneBillingRepository returns a [ControlPlaneOption] that uses the provided BillingRepository.
func WithControlPlaneBillingRepository(repository trustedai.BillingRepository) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.BillingRepository = repository
	})
}

// WithSSOHandler returns a [ControlPlaneOption] that uses the provided SSO handler.
func WithSSOHandler(handler http.Handler) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.SsoHandler = handler
	})
}

// WithSessionStore returns a [ControlPlaneOption] that uses the provided session store.
func WithSessionStore(store auth.SessionStore) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.SessionStore = store
	})
}

// WithAuthInterceptor returns a [ControlPlaneOption] that uses the provided auth interceptor.
func WithAuthInterceptor(interceptor *cauth.Interceptor) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.AuthInterceptor = interceptor
	})
}

// WithTokenInterceptor returns a [ControlPlaneOption] that uses the provided token interceptor.
func WithTokenInterceptor(interceptor *cauth.TokenInterceptor) ControlPlaneOption {
	return newFuncControlPlaneOption(func(opts *controlPlaneOptions) {
		opts.TokenInterceptor = interceptor
	})
}

// GetMux returns the HTTP mux for the server
func (s *ControlPlaneServer) GetMux() *http.ServeMux {
	return s.mux
}

func (s *ControlPlaneServer) Run() {
	fmt.Println("Starting server on localhost:9999")

	// Simple CORS middleware wrapper
	corsWrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("CORS Middleware: %s %s from Origin: %s\n", r.Method, r.URL.Path, r.Header.Get("Origin"))

		// Set CORS headers for all requests
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin")

		// Handle preflight OPTIONS requests
		if r.Method == http.MethodOptions {
			fmt.Printf("CORS Middleware: Handling OPTIONS preflight request\n")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Authorization, X-User-Agent, User-Agent, Accept-Encoding")
			w.Header().Set("Access-Control-Max-Age", "7200")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Pass through to original handler
		s.mux.ServeHTTP(w, r)
	})

	// Apply h2c to CORS wrapper
	handler := h2c.NewHandler(corsWrapper, &http2.Server{})

	_ = http.ListenAndServe("localhost:9999", handler)
}
