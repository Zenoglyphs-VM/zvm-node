package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/verify"
)

func compilerRemoteRecord(t *testing.T, name string, extra map[string]any) {
	t.Helper()
	record := map[string]any{"case": name, "actual_production_service_lifetimes": 1, "joined_services": 1,
		"same_Node_RemoteRunner_used": true, "automatic_retry_or_local_fallback": false}
	for key, value := range extra {
		record[key] = value
	}
	compilerProfileRecord(t, record)
}

func TestCompilerRemoteClientContainerProfile(t *testing.T) {
	if os.Getenv("ZVM_N01_CONTAINER_PROFILE") != "1" {
		t.Skip("requires the explicit actual solcd kernel-isolation job")
	}
	binary := os.Getenv("ZVM_N01_SOLCD_BINARY")
	if binary != "/qualified/zvm-node" {
		t.Fatal("declared production binary missing")
	}
	compilerProfileKernel(t)
	before := compilerProfileOOM(t)
	input := []byte(`{"language":"Solidity","sources":{}}`)
	t.Run("typed_service_output_limit", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "stdout")
		p := f.start(binary, 2)
		client := &verify.RemoteRunner{URL: p.url, MaxOutputBytes: 65536}
		for attempt := 0; attempt < 2; attempt++ {
			out, err := client.Compile(context.Background(), f.version, input)
			if out != nil || !errors.Is(err, verify.ErrCompilerOutputLimit) || strings.Contains(err.Error(), p.url) || strings.Contains(err.Error(), f.dir) {
				t.Fatal("actual service output refusal lost its typed private boundary", err)
			}
		}
		p.stop(t)
		compilerRemoteRecord(t, "actual_remote_service_output_limit", map[string]any{"attempts": 2, "typed_output_limit": true, "partial_success": false, "slot_reused": true})
	})
	t.Run("typed_cache_full_and_retained_binary", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "success")
		p := f.start(binary, 1)
		client := &verify.RemoteRunner{URL: p.url}
		if _, err := client.Compile(context.Background(), f.second, input); !errors.Is(err, verify.ErrCompilerCacheFull) {
			t.Fatal("actual cache pressure lost its client type", err)
		}
		out, err := client.Compile(context.Background(), f.version, input)
		retained, readErr := os.ReadFile(filepath.Join(f.dir, "solc-"+f.version))
		if err != nil || !bytes.Contains(out, []byte(`"contracts"`)) || readErr != nil || !bytes.Equal(retained, f.body) || f.binaryReads.Load() != 0 {
			t.Fatal("cache refusal changed the original binary or prevented independent retained work", err)
		}
		p.stop(t)
		compilerRemoteRecord(t, "actual_remote_cache_pressure", map[string]any{"typed_cache_full": true, "existing_binary_unchanged": true, "new_binary_downloads": 0, "independent_retained_compile_succeeded": true})
	})
	t.Run("typed_busy_deadline_and_fresh_success", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "deadline")
		// The retained compiler, its owned start marker and the second compiler
		// each occupy one cache file. Keep all three in the explicit budget.
		p := f.start(binary, 3)
		client := &verify.RemoteRunner{URL: p.url}
		done := make(chan error, 1)
		go func() { _, err := client.Compile(context.Background(), f.version, input); done <- err }()
		limit := time.Now().Add(2 * time.Second)
		marked := false
		for time.Now().Before(limit) {
			if _, err := os.Stat(f.marker); err == nil {
				marked = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !marked {
			t.Fatal("actual controlled compiler did not start")
		}
		if _, err := client.Compile(context.Background(), f.version, input); !errors.Is(err, verify.ErrCompilerBusy) {
			t.Fatal("actual busy service lost its client type", err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("actual compiler deadline lost its client type", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("actual deadline client did not join")
		}
		compilerProfileGroupStopped(t, f.marker)
		if out, err := client.Compile(context.Background(), f.second, input); err != nil || !bytes.Contains(out, []byte(`"contracts"`)) || f.binaryReads.Load() != 1 {
			t.Fatal("deadline retained the slot, changed release work or canceled independent later work", err)
		}
		p.stop(t)
		compilerRemoteRecord(t, "actual_remote_busy_deadline_and_reuse", map[string]any{"actual_same_group_compiler_children_stopped": 1, "original_stop_observation_bound_ms": 300, "escaped_or_adopted_descendants_qualified": false, "typed_busy": true, "typed_deadline": true, "original_client_joined": true, "independent_later_compile_succeeded": true, "new_binary_downloads": 1, "cache_file_budget": 3, "owned_start_marker_files": 1})
	})
	t.Run("client_success_output_budget", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "large-success")
		p := f.start(binary, 2)
		client := &verify.RemoteRunner{URL: p.url, MaxOutputBytes: 1024}
		if out, err := client.Compile(context.Background(), f.version, input); out != nil || !errors.Is(err, verify.ErrCompilerOutputLimit) {
			t.Fatal("successful large service output became partial client success", err)
		}
		later := &verify.RemoteRunner{URL: p.url, MaxOutputBytes: 8192}
		if out, err := later.Compile(context.Background(), f.version, input); err != nil || len(out) <= 1024 || len(out) > 8192 {
			t.Fatal("explicit fitting client output budget did not complete", err)
		}
		p.stop(t)
		compilerRemoteRecord(t, "actual_remote_success_output_budget", map[string]any{"client_output_budget_bytes": 1024, "typed_output_limit": true, "partial_success": false, "independent_fitting_budget_succeeded": true})
	})
	t.Run("version_refusal_precedes_release_work", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "success")
		p := f.start(binary, 1)
		version := strings.Repeat("v", verify.MaxCompilerVersionBytes+1)
		client := &verify.RemoteRunner{URL: p.url}
		if _, err := client.Compile(context.Background(), version, input); !errors.Is(err, verify.ErrInvalid) {
			t.Fatal("client did not bound its version name", err)
		}
		status, _, err := compilerProfileRequest(p, version)
		if err != nil || status != 400 || f.listReads.Load() != 0 || f.binaryReads.Load() != 0 {
			t.Fatal("actual service invalid version reached registry or binary work", err)
		}
		if _, err := client.Compile(context.Background(), f.version, input); err != nil {
			t.Fatal("invalid version retained a slot or disabled known compiler", err)
		}
		p.stop(t)
		compilerRemoteRecord(t, "actual_remote_version_limit", map[string]any{"max_version_bytes": 128, "typed_invalid": true, "invalid_service_HTTP_status": 400, "release_requests_before_refusal": 0, "binary_requests_before_refusal": 0, "independent_later_compile_succeeded": true})
	})
	after := compilerProfileOOM(t)
	if after["oom"] != before["oom"] || after["oom_kill"] != before["oom_kill"] {
		t.Fatal("remote compiler boundary profile caused an OOM event")
	}
	compilerProfileRecord(t, map[string]any{"case": "compiler_remote_kernel_profile", "memory_max_bytes": 512 << 20, "swap_max_bytes": 0, "CPUs": 2, "pids_max": 64, "tmpfs_bytes": 64 << 20,
		"uid": 10001, "gid": 10001, "read_only_root": true, "network": "none", "effective_capabilities_zero": true, "no_new_privileges": true,
		"OOM_events": 0, "actual_production_service_lifetimes": 5, "joined_services": 5, "automatic_retry_or_local_fallback": false,
		"real_node_keys_state_or_live_chain_used": false, "actual_solc_vendor_binary_qualified": false})
}
