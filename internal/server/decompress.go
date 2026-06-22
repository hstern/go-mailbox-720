package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/hstern/go-mailbox-720/internal/grapherr"
)

// maxDecompressedBytes caps the inflated size of a gzip request body. Without it
// a small gzip payload could decompress to gigabytes (a "gzip bomb") and OOM the
// server, since downstream JSON decoding reads without its own bound. It matches
// the $batch raw-body cap (batch.maxBatchBytes); a nested cap inside the batch
// handler is harmless.
const maxDecompressedBytes = 4 << 20 // 4 MiB

// DecompressRequests wraps next so a request carrying "Content-Encoding: gzip"
// has its body transparently decompressed before next reads it.
//
// Microsoft's official msgraph-sdk-go compresses request bodies by default, so
// the conformance harness — and real Graph clients — send gzip-encoded POST /
// PATCH bodies (notably $batch) that the handlers would otherwise try to parse
// as raw gzip and reject. A body whose declared gzip framing is malformed is
// answered with a Graph-shaped 400 before reaching next; an over-large inflated
// body is capped (downstream reads then fail) to bound memory.
//
// It is mounted as the outermost middleware (ahead of auth and the mux) so every
// body-bearing endpoint benefits; auth does not read the body, so the ordering
// is immaterial to it. Sub-requests inside a $batch carry no encoding of their
// own — they are plain JSON within the already-decompressed envelope — so this
// one outer layer is sufficient.
func DecompressRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				grapherr.Write(w, http.StatusBadRequest)
				return
			}
			// Bound the inflated stream against gzip bombs; MaxBytesReader makes
			// reads past the cap fail, so a downstream decode errors out instead
			// of allocating unbounded memory.
			r.Body = http.MaxBytesReader(w, gzipBody{zr: zr, orig: r.Body}, maxDecompressedBytes)
			// The decoded length is unknown and no longer gzip-encoded; clear the
			// stale framing so downstream handlers don't trust it.
			r.Header.Del("Content-Encoding")
			r.Header.Del("Content-Length")
			r.ContentLength = -1
		}
		next.ServeHTTP(w, r)
	})
}

// gzipBody adapts a gzip.Reader over the original body, closing both so the
// underlying connection is still drained/reused.
type gzipBody struct {
	zr   *gzip.Reader
	orig io.ReadCloser
}

func (g gzipBody) Read(p []byte) (int, error) { return g.zr.Read(p) }

func (g gzipBody) Close() error {
	err := g.zr.Close()
	if cerr := g.orig.Close(); err == nil {
		err = cerr
	}
	return err
}
