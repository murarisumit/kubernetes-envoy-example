package order

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bakins/kubernetes-envoy-example/api/item"
	"github.com/bakins/kubernetes-envoy-example/api/order"
	"github.com/bakins/kubernetes-envoy-example/util"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_validator "github.com/grpc-ecosystem/go-grpc-middleware/validator"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/hkwi/h2c"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

func init() {
	grpc_prometheus.EnableHandlingTimeHistogram()
}

// OptionsFunc sets options when creating a new server.
type OptionsFunc func(*Server) error

// Server is a wrapper for a simple front end HTTP server
type Server struct {
	address  string
	endpoint string
	server   *http.Server
	grpc     *grpc.Server
	store    *orderStore
	item     item.ItemServiceClient
}

// New creates a new server
func New(options ...OptionsFunc) (*Server, error) {
	s := &Server{
		address:  ":8080",
		endpoint: "127.0.0.1:9090",
	}

	for _, f := range options {
		if err := f(s); err != nil {
			return nil, errors.Wrap(err, "options function failed")
		}
	}

	ctx := context.Background()
	conn, err := grpc.DialContext(
		ctx,
		s.endpoint,
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(grpc_middleware.ChainUnaryClient(
			util.UnaryClientInterceptor(),
			grpc_prometheus.UnaryClientInterceptor,
		)),
		grpc.WithStreamInterceptor(grpc_middleware.ChainStreamClient(
			grpc_prometheus.StreamClientInterceptor,
		)),
	)

	if err != nil {
		return nil, errors.Wrap(err, "could not create grpc client")
	}
	s.item = item.NewItemServiceClient(conn)
	s.store = newOrderStore(s, s.item)
	// TODO: option to load this or not
	s.store.LoadSampleData()

	return s, nil
}

// SetAddress sets the listening address.
func SetAddress(address string) OptionsFunc {
	return func(s *Server) error {
		s.address = address
		return nil
	}
}

// SetEndpoint sets the address for contacting other services.
func SetEndpoint(address string) OptionsFunc {
	return func(s *Server) error {
		s.endpoint = address
		return nil
	}
}

// Run starts the server. This generally does not return.
func (s *Server) Run() error {
	logger, err := util.NewDefaultLogger()
	if err != nil {
		return errors.Wrapf(err, "failed to create logger")
	}

	l, err := net.Listen("tcp", s.address)
	if err != nil {
		return errors.Wrapf(err, "failed to listen on %s", s.address)
	}

	grpc_zap.ReplaceGrpcLogger(logger)
	grpc_prometheus.EnableHandlingTimeHistogram()

	s.grpc = grpc.NewServer(
		grpc.UnaryInterceptor(
			grpc_middleware.ChainUnaryServer(
				util.UnaryServerInterceptor(),
				util.UnaryServerSleeperInterceptor(time.Second*3),
				grpc_validator.UnaryServerInterceptor(),
				grpc_prometheus.UnaryServerInterceptor,
				grpc_zap.UnaryServerInterceptor(logger),
				grpc_recovery.UnaryServerInterceptor(),
			),
		),
	)

	gwmux := runtime.NewServeMux()
	_, port, err := net.SplitHostPort(s.address)
	if err != nil {
		return errors.Wrapf(err, "invalid address %s", s.address)
	}

	if err := order.RegisterOrderServiceHandlerFromEndpoint(context.Background(), gwmux, net.JoinHostPort("127.0.0.1", port), []grpc.DialOption{grpc.WithInsecure()}); err != nil {
		return errors.Wrap(err, "failed to register grpc gateway")
	}

	order.RegisterOrderServiceServer(s.grpc, s.store)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/", gwmux)

	s.server = &http.Server{
		Handler: h2c.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor == 2 &&
					strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
					s.grpc.ServeHTTP(w, r)
				} else {
					mux.ServeHTTP(w, r)
				}
			}),
		},
	}

	if err := s.server.Serve(l); err != nil {
		if err != http.ErrServerClosed {
			return errors.Wrap(err, "failed to start http server")
		}
	}

	return nil
}

// Stop will stop the server
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	s.server.Shutdown(ctx)
}

func healthz(wr http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(wr, "OK\n")
}
