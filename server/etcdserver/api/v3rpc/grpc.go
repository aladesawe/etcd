// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v3rpc

import (
	"crypto/tls"
	"math"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/v3/credentials"
	"go.etcd.io/etcd/server/v3/etcdserver"
)

const (
	maxSendBytes = math.MaxInt32
)

func Server(s *etcdserver.EtcdServer, tls *tls.Config, interceptor grpc.UnaryServerInterceptor, gopts ...grpc.ServerOption) *grpc.Server {
	var opts []grpc.ServerOption
	opts = append(opts, grpc.CustomCodec(&codec{}))
	if tls != nil {
		opts = append(opts, grpc.Creds(credentials.NewTransportCredential(tls)))
	}

	serverMetrics := grpc_prometheus.NewServerMetrics()
	err := prometheus.Register(serverMetrics)
	if err != nil {
		s.Cfg.Logger.Warn("etcdserver: failed to register grpc metrics: ", zap.Error(err))
	}

	chainUnaryInterceptors := []grpc.UnaryServerInterceptor{
		newLogUnaryInterceptor(s),
		newUnaryInterceptor(s),
		serverMetrics.UnaryServerInterceptor(),
	}
	if interceptor != nil {
		chainUnaryInterceptors = append(chainUnaryInterceptors, interceptor)
	}

	chainStreamInterceptors := []grpc.StreamServerInterceptor{
		newStreamInterceptor(s),
		serverMetrics.StreamServerInterceptor(),
	}

	// If extensive metrics are enabled, register a histogram to track the reponse latency of gRPC requests
	if s.Cfg.Metrics == "extensive" {
		unaryInterceptor, streamInterceptor := constructExtensiveMetricsInterceptors()
		chainUnaryInterceptors = append(chainUnaryInterceptors, unaryInterceptor)
		chainStreamInterceptors = append(chainStreamInterceptors, streamInterceptor)
	}

	if s.Cfg.ExperimentalEnableDistributedTracing {
		chainUnaryInterceptors = append(chainUnaryInterceptors, otelgrpc.UnaryServerInterceptor(s.Cfg.ExperimentalTracerOptions...))
		chainStreamInterceptors = append(chainStreamInterceptors, otelgrpc.StreamServerInterceptor(s.Cfg.ExperimentalTracerOptions...))
	}

	opts = append(opts, grpc.ChainUnaryInterceptor(chainUnaryInterceptors...))
	opts = append(opts, grpc.ChainStreamInterceptor(chainStreamInterceptors...))

	opts = append(opts, grpc.MaxRecvMsgSize(int(s.Cfg.MaxRequestBytesWithOverhead())))
	opts = append(opts, grpc.MaxSendMsgSize(maxSendBytes))
	opts = append(opts, grpc.MaxConcurrentStreams(s.Cfg.MaxConcurrentStreams))

	grpcServer := grpc.NewServer(append(opts, gopts...)...)

	pb.RegisterKVServer(grpcServer, NewQuotaKVServer(s))
	pb.RegisterWatchServer(grpcServer, NewWatchServer(s))
	pb.RegisterLeaseServer(grpcServer, NewQuotaLeaseServer(s))
	pb.RegisterClusterServer(grpcServer, NewClusterServer(s))
	pb.RegisterAuthServer(grpcServer, NewAuthServer(s))

	hsrv := health.NewServer()
	healthNotifier := newHealthNotifier(hsrv, s)
	healthpb.RegisterHealthServer(grpcServer, hsrv)
	pb.RegisterMaintenanceServer(grpcServer, NewMaintenanceServer(s, healthNotifier))

	// set zero values for metrics registered for this grpc server
	serverMetrics.InitializeMetrics(grpcServer)

	return grpcServer
}
