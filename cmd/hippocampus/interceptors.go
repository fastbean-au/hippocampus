package main

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

func InterceptorLogger(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	log.Tracef("gRPC -> %s", info.FullMethod)

	ts := time.Now()

	resp, err := handler(ctx, req)

	log.Tracef("gRPC <- %s (%d us)", info.FullMethod, time.Since(ts).Microseconds())

	return resp, err
}
