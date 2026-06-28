// Copyright (c) 2026, the wasmdesk/wasmdesk authors. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// WaitForHTTP polls url every 200 ms until pingOnce succeeds or the timeout
// elapses. Returns nil on success, an error describing the last failure
// otherwise.
func WaitForHTTP(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := pingOnce(ctx, url); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s: %w", url, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// WaitForAll polls every url in parallel until all return 200, the context is
// done, or the timeout elapses. Returns nil only when all are up.
func WaitForAll(ctx context.Context, urls []string, timeout time.Duration) error {
	if len(urls) == 0 {
		return nil
	}
	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, len(urls))
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			errs[i] = WaitForHTTP(subCtx, u, timeout)
		}(i, u)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("%s: %w", urls[i], err)
		}
	}
	return nil
}
