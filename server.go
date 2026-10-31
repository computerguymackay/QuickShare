package main

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// FileServer serves shares by token and counts completed downloads.
//
// Counting rule: a GET without a Range header that sends the whole file
// with no write error counts once. Range requests (resumes, partial
// fetches) and HEAD requests never count.
type FileServer struct {
	store *Store
	log   *log.Logger
	now   func() time.Time

	inflight atomic.Int64
	// onChange runs after any request finishes, so the daemon can
	// re-check whether it should shut down.
	onChange func()
}

func (fs *FileServer) InFlight() int64 { return fs.inflight.Load() }

func (fs *FileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fs.inflight.Add(1)
	defer func() {
		fs.inflight.Add(-1)
		if fs.onChange != nil {
			fs.onChange()
		}
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// URL shape: /<token> or /<token>/<filename>
	token, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	gone := func(reason string) {
		fs.log.Printf("%s %s %s → 410 (%s)", r.Method, r.RemoteAddr, shortToken(token), reason)
		http.Error(w, "this link has expired or does not exist", http.StatusGone)
	}
	if token == "" {
		gone("no token")
		return
	}

	sh, err := fs.store.Get(token)
	if err != nil {
		gone("unknown")
		return
	}
	if !sh.Active(fs.now()) {
		gone("expired or used up")
		return
	}

	f, err := os.Open(sh.Path)
	if err != nil {
		fs.store.Delete(token)
		gone("file missing")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		fs.store.Delete(token)
		gone("file missing")
		return
	}

	name := filepath.Base(sh.Path)
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))

	cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
	http.ServeContent(cw, r, name, fi.ModTime(), f)

	counted := r.Method == http.MethodGet &&
		r.Header.Get("Range") == "" &&
		cw.status == http.StatusOK &&
		cw.err == nil &&
		cw.n == fi.Size() &&
		r.Context().Err() == nil

	note := ""
	if counted {
		after, err := fs.store.Increment(token)
		if err != nil {
			fs.log.Printf("count update failed for %s: %v", shortToken(token), err)
		} else {
			note = " counted " + countString(after)
		}
	}
	fs.log.Printf("%s %s %s → %d %d/%d bytes%s", r.Method, r.RemoteAddr, shortToken(token), cw.status, cw.n, fi.Size(), note)
}

func countString(sh Share) string {
	if sh.MaxDownloads == 0 {
		return itoa(sh.Count) + "/∞"
	}
	return itoa(sh.Count) + "/" + itoa(sh.MaxDownloads)
}

// countingWriter records the status, body bytes, and any write error.
type countingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	n           int64
	err         error
}

func (c *countingWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.status = code
		c.wroteHeader = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.wroteHeader = true
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

// ReadFrom keeps the sendfile fast path of the underlying writer.
func (c *countingWriter) ReadFrom(r io.Reader) (int64, error) {
	c.wroteHeader = true
	rf, ok := c.ResponseWriter.(io.ReaderFrom)
	var n int64
	var err error
	if ok {
		n, err = rf.ReadFrom(r)
	} else {
		n, err = io.Copy(struct{ io.Writer }{c.ResponseWriter}, r)
	}
	c.n += n
	if err != nil && c.err == nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
