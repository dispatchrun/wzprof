package wzprof

import (
	"fmt"
	"net/http"

	"github.com/google/pprof/profile"
)

// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

func serveProfile(w http.ResponseWriter, prof *profile.Profile) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="profile"`)
	if err := prof.Write(w); err != nil {
		serveError(w, http.StatusInternalServerError, err.Error())
	}
}

func serveError(w http.ResponseWriter, status int, txt string) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Go-Pprof", "1")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Del("Content-Disposition")
	w.WriteHeader(status)
	fmt.Fprintln(w, txt)
}
