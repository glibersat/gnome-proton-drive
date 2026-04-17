package rpc

import (
	"bufio"
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

	enc := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)

	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = enc.Encode(Response{Error: &RPCError{Code: ErrInternal, Message: "malformed request"}})
			continue
		}

		resp := s.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			log.Printf("write response: %v", err)
			return
		}
	}
}

func (s *Server) dispatch(req Request) Response {
	h, ok := s.handlers[req.Method]
	if !ok {
		return Response{ID: req.ID, Error: &RPCError{Code: ErrInternal, Message: "unknown method: " + req.Method}}
	}

	result, err := h(context.Background(), req.Params)
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
