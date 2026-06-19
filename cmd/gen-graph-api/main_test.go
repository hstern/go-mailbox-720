package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFetchSpec(t *testing.T) {
	const body = "openapi: 3.0.4\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "openapi-full.yaml")
	if err := fetchSpec(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("fetchSpec: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestFetchSpecNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "openapi-full.yaml")
	if err := fetchSpec(context.Background(), srv.URL, dest); err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestFetchSpecPreservesCacheOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	const cached = "previously fetched spec\n"
	dest := filepath.Join(t.TempDir(), "openapi-full.yaml")
	if err := os.WriteFile(dest, []byte(cached), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fetchSpec(context.Background(), srv.URL, dest); err == nil {
		t.Fatal("expected error, got nil")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != cached {
		t.Errorf("cache clobbered on failed fetch: got %q, want %q", got, cached)
	}
}

func TestOgenArgs(t *testing.T) {
	got := ogenArgs("api", "api", "openapi-subset.yaml")
	want := []string{
		"run", ogenPkg + "@" + ogenVersion,
		"--target", "api",
		"--package", "api",
		"--clean",
		"openapi-subset.yaml",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ogenArgs() = %v, want %v", got, want)
	}
}
