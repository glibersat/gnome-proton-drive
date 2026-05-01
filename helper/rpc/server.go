package rpc

import (
	"context"
	"encoding/json"
	"log"
	"net"
)

type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type Server struct {
	handlers map[string]Handler
}

func NewServer() *Server {
	return &Server{handlers: make(map[string]Handler)}
}

func (s *Server) Register(method string, h Handler) {
	s.handlers[method] = h
}

func (s *Server) Serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())

	enc := json.NewEncoder(conn)
	reqs := make(chan Request)

	// Reader goroutine: decodes newline-delimited JSON requests and cancels
	// ctx when the connection closes, propagating into any in-flight handler.
	// json.Decoder has no line-length limit, unlike bufio.Scanner (64 KiB
	// default), which is necessary for WriteFile with large payloads.
	go func() {
		defer cancel()
		defer close(reqs)
		dec := json.NewDecoder(conn)
		for {
			var req Request
			if err := dec.Decode(&req); err != nil {
				return
			}
			reqs <- req
		}
	}()

	for req := range reqs {
		resp := s.dispatch(ctx, req)
		if err := enc.Encode(resp); err != nil {
			log.Printf("write response: %v", err)
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	h, ok := s.handlers[req.Method]
	if !ok {
		return Response{ID: req.ID, Error: &RPCError{Code: ErrInternal, Message: "unknown method: " + req.Method}}
	}

	result, err := h(ctx, req.Params)
	if err != nil {
		if rpcErr, ok := err.(*RPCError); ok {
			return Response{ID: req.ID, Error: rpcErr}
		}
		return Response{ID: req.ID, Error: &RPCError{Code: ErrInternal, Message: err.Error()}}
	}

	return Response{ID: req.ID, Result: result}
}

func NotFound(msg string) *RPCError {
	return &RPCError{Code: ErrNotFound, Message: msg}
}

func NotAuth() *RPCError {
	return &RPCError{Code: ErrNotAuthed, Message: "not authenticated"}
}

func Offline(msg string) *RPCError {
	return &RPCError{Code: ErrOffline, Message: msg}
}
