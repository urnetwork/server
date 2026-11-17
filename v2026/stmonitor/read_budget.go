// Read retries retain one source and one operation deadline. Each prior
// connection has physically joined before the next independent snapshot begins.
package stmonitor

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Package-private hooks can observe actual owned transactions or withhold a
// read. They cannot provide a snapshot, connection, or successful authority.
type readObservationHooks struct {
	now        func() time.Time
	wait       func(context.Context, time.Duration) error
	afterBegin func(context.Context, *pgx.Conn, pgx.Tx) error
}

// Only tests inside this package can install the observation contract.
type readObservationKey struct{}

// Waiting retains the original deadline and is canceled by the same caller.
func waitReadRetry(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// The finite tree walk trusts exact local causes and closed PostgreSQL codes.
// Completed identity/capacity/shape refusals, arbitrary custom Is/As, unknown
// SQL errors, typed nils and recursive wrappers cannot borrow a soft cause.
func retryableRead(err error) bool {
	nodes := 0
	var visit func(error, int) bool
	visit = func(cause error, depth int) bool {
		nodes++
		if cause == nil || depth >= 32 || nodes > 128 {
			return false
		}
		value := reflect.ValueOf(cause)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if cause == context.Canceled || cause == context.DeadlineExceeded || cause == io.EOF || cause == io.ErrUnexpectedEOF {
			return true
		}
		switch typed := cause.(type) {
		case syscall.Errno:
			switch typed {
			case syscall.EAGAIN, syscall.EBUSY, syscall.EIO, syscall.EINTR, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM, syscall.ENOSPC, syscall.EDQUOT,
				syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT:
				return true
			}
			return false
		case *ReadError:
			return typed.Code == "unavailable" && visit(typed.cause, depth+1)
		case *pgconn.PgError:
			switch typed.Code {
			case "08000", "08001", "08003", "08006", "08007", "40001", "40P01", "53100", "53200", "53300", "53400", "55P03", "57014", "57P01", "57P02", "57P03":
				return true
			}
			return false
		case *net.DNSError:
			if underlying := typed.Unwrap(); underlying != nil {
				return visit(underlying, depth+1)
			}
			return typed.IsTimeout || typed.IsTemporary
		case interface{ Is(error) bool }, interface{ As(any) bool }:
			return false
		}
		if many, ok := cause.(interface{ Unwrap() []error }); ok {
			children := many.Unwrap()
			if len(children) == 0 || len(children) > 128-nodes {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if one, ok := cause.(interface{ Unwrap() error }); ok {
			return visit(one.Unwrap(), depth+1)
		}
		return false
	}
	return visit(err, 0)
}

// A completed local refusal remains the outer classification, so a prior
// unavailable attempt cannot mask it in the caller's closed metric projection.
func readRefusalCode(err error) string {
	code, nodes := "unavailable", 0
	var visit func(error, int)
	visit = func(cause error, depth int) {
		nodes++
		if cause == nil || depth >= 32 || nodes > 128 {
			return
		}
		value := reflect.ValueOf(cause)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return
			}
		}
		if typed, ok := cause.(*ReadError); ok && typed != nil {
			switch typed.Code {
			case "identity":
				code = "identity"
			case "invalid", "capacity":
				if code != "identity" {
					code = typed.Code
				}
			}
			visit(typed.cause, depth+1)
			return
		}
		switch cause.(type) {
		case interface{ Is(error) bool }, interface{ As(any) bool }:
			return
		}
		if many, ok := cause.(interface{ Unwrap() []error }); ok {
			children := many.Unwrap()
			if len(children) > 128-nodes {
				return
			}
			for _, child := range children {
				visit(child, depth+1)
			}
		} else if one, ok := cause.(interface{ Unwrap() error }); ok {
			visit(one.Unwrap(), depth+1)
		}
	}
	visit(err, 0)
	return code
}

// First and last causes suffice to preserve evidence without growing an error
// chain for every retry. The finite attempt cap also bounds test wait hooks.
func readWithBudget(parent context.Context, cfg *pgx.ConnConfig, source Source) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(parent, ReadTimeout)
	defer cancel()
	hooks, _ := parent.Value(readObservationKey{}).(readObservationHooks)
	now, wait := time.Now, waitReadRetry
	if hooks.now != nil {
		now = hooks.now
	}
	if hooks.wait != nil {
		wait = hooks.wait
	}
	deadline := now().Add(ReadTimeout)
	if callerDeadline, ok := parent.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	delay := time.Second
	var firstErr, lastErr error
	for attempt := 0; attempt < 64 && ctx.Err() == nil && now().Before(deadline); attempt++ {
		value, err := readAttempt(ctx, cfg, source, hooks)
		if err == nil && ctx.Err() == nil && now().Before(deadline) {
			return value, nil
		}
		if err != nil {
			if !retryableRead(err) {
				return nil, refuse(readRefusalCode(err), errors.Join(firstErr, err))
			}
			if firstErr == nil {
				firstErr = err
			}
			lastErr = err
		}
		if ctx.Err() != nil || !now().Before(deadline) {
			break
		}
		if err := wait(ctx, min(delay, deadline.Sub(now()))); err != nil {
			return nil, refuse(readRefusalCode(err), errors.Join(firstErr, lastErr, err, ctx.Err()))
		}
		delay = min(2*delay, 10*time.Second)
	}
	cause := ctx.Err()
	if cause == nil {
		cause = context.DeadlineExceeded
	}
	return nil, refuse("unavailable", errors.Join(firstErr, lastErr, cause))
}
