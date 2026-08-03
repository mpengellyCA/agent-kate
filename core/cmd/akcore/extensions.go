package main

// The extension-catalogue RPCs live outside handlers.go so the legacy
// skills.* implementation remains mechanically unchanged.  Plugin commands
// all mutate a user-level CLI configuration or read it by spawning a CLI, so
// they are deliberately UI-only (the same authority boundary as skills.*).

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"agentkate/internal/extensions"
	"agentkate/internal/ipc"
	"agentkate/internal/safe"
)

const extensionCLITimeout = 30 * time.Second

// extensionCall is the one containment boundary for all external plugin CLI
// calls. It preserves a response for the RPC while safe.Go contains a panic in
// a background invocation. The buffered channel means a timed-out caller never
// strands the worker trying to report its result.
func extensionCall[T any](ctx context.Context, label string, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, extensionCLITimeout)
	defer cancel()
	type result struct {
		value T
		err   error
	}
	ch := make(chan result, 1)
	safe.Go("extensions."+label, func() { value, err := fn(ctx); ch <- result{value, err} })
	select {
	case r := <-ch:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func registerExtensionHandlers(d handlerDeps) {
	plugins := d.claudePlugins
	if plugins == nil {
		plugins = extensions.NewClaudePlugins("claude")
	}

	// call confines each CLI invocation to a safe goroutine and a deadline. A
	// bad marketplace or a wedged CLI becomes a named source error in a reply;
	// it can never take the core down or make the catalogue unavailable.
	call := func(label string, fn func(context.Context) ([]extensions.Extension, error)) ([]extensions.Extension, string) {
		entries, err := extensionCall(context.Background(), label, fn)
		if err != nil {
			return []extensions.Extension{}, err.Error()
		}
		return entries, ""
	}

	// extensions.list intentionally returns all independent sources even when
	// one is down. Loose Skills remain the authoritative local fallback.
	d.srv.Handle("extensions.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var catalog []extensions.Extension
		if d.skills != nil {
			if list, err := d.skills.List(); err == nil {
				catalog = extensions.FromSkills(list)
			}
		}
		var installed, available []extensions.Extension
		var installedErr, availableErr string
		var wg sync.WaitGroup
		wg.Add(2)
		safe.Go("extensions.listInstalled", func() { defer wg.Done(); installed, installedErr = call("listInstalled", plugins.ListInstalled) })
		safe.Go("extensions.listAvailable", func() { defer wg.Done(); available, availableErr = call("listAvailable", plugins.ListAvailable) })
		wg.Wait()
		return map[string]any{"catalog": catalog, "installed": installed, "available": available,
			"errors": map[string]string{"installed": installedErr, "available": availableErr}}, nil
	})

	d.srv.Handle("extensions.components", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		e, err := extensionCall(context.Background(), "details", func(callCtx context.Context) (extensions.Extension, error) { return plugins.Details(callCtx, p.Name) })
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return e, nil
	})

	type nameParams struct {
		Name string `json:"name"`
	}
	mutate := func(method string, fn func(context.Context, string) error) {
		d.srv.Handle(method, func(ctx context.Context, raw json.RawMessage) (any, error) {
			if err := requireUIWindow(d.srv, ctx); err != nil {
				return nil, err
			}
			var p nameParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
			}
			_, err := extensionCall(context.Background(), method, func(callCtx context.Context) (struct{}, error) { return struct{}{}, fn(callCtx, p.Name) })
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
			}
			return map[string]any{"ok": true}, nil
		})
	}
	mutate("extensions.install", plugins.Install)
	mutate("extensions.uninstall", plugins.Uninstall)
	mutate("extensions.enable", plugins.Enable)
	mutate("extensions.disable", plugins.Disable)

	d.srv.Handle("extensions.marketplaces", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		marketplaces, err := extensionCall(context.Background(), "marketplaces", plugins.Marketplaces)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"marketplaces": marketplaces}, nil
	})
	d.srv.Handle("extensions.addMarketplace", func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := requireUIWindow(d.srv, ctx); err != nil {
			return nil, err
		}
		var p struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		_, err := extensionCall(context.Background(), "addMarketplace", func(callCtx context.Context) (struct{}, error) {
			return struct{}{}, plugins.AddMarketplace(callCtx, p.Source)
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})
}
