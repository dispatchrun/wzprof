package wzprof

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"net/http"
	httpprof "net/http/pprof"
	"net/url"
	"runtime/pprof"
	"sort"
	"strings"

	"github.com/google/pprof/profile"
)

// Copyright (c) 2009 The Go Authors. All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google Inc. nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

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

type profileEntry struct {
	Name    string
	Href    string
	Desc    string
	Debug   int
	Count   int
	Handler http.Handler
}

func sortProfiles(entries []profileEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
}

// Index responds with the pprof-formatted profile named by the request.
// For example, "/debug/pprof/heap" serves the "heap" profile.
// Index responds to a request for "/debug/pprof/" with an HTML page
// listing the available profiles.
func Index(sampleRate float64, symbols Symbolizer, profilers ...Profiler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var guest, host []profileEntry

		for _, p := range profilers {
			guest = append(guest, profileEntry{
				Name:    p.Name(),
				Href:    p.Name(),
				Desc:    p.Desc(),
				Count:   p.Count(),
				Handler: p.NewHandler(sampleRate, symbols),
			})
		}

		// Add host profiling debug entries.
		host = append(host, profileEntry{
			Name:    "cmdline",
			Href:    "cmdline",
			Desc:    profileDescriptions["cmdline"],
			Handler: http.HandlerFunc(httpprof.Cmdline),
			Debug:   1,
		})

		host = append(host, profileEntry{
			Name:    "profile",
			Href:    "profile",
			Desc:    profileDescriptions["profile"],
			Handler: http.HandlerFunc(httpprof.Profile),
			Debug:   1,
		})

		host = append(host, profileEntry{
			Name:    "trace",
			Href:    "trace",
			Desc:    profileDescriptions["trace"],
			Handler: http.HandlerFunc(httpprof.Trace),
			Debug:   1,
		})

		for _, p := range pprof.Profiles() {
			host = append(host, profileEntry{
				Name:    p.Name(),
				Href:    p.Name(),
				Desc:    profileDescriptions[p.Name()],
				Count:   p.Count(),
				Handler: httpprof.Handler(p.Name()),
				Debug:   1,
			})
		}

		p := pprof.Lookup("goroutine")
		host = append(host, profileEntry{
			Name:    "full goroutine stack dump",
			Href:    p.Name(),
			Count:   p.Count(),
			Handler: httpprof.Handler(p.Name()),
			Debug:   2,
		})

		if href, found := strings.CutPrefix(r.URL.Path, "/debug/pprof/"); found {
			var entries []profileEntry
			_, queryHost := r.URL.Query()["host"]
			if queryHost {
				entries = host
			} else {
				entries = guest
			}
			for _, entry := range entries {
				if entry.Href == href {
					entry.Handler.ServeHTTP(w, r)
					return
				}
			}
		}

		sortProfiles(guest)
		sortProfiles(host)

		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Type", "text/html; charset=utf-8")

		if err := indexTmplExecute(w, guest, host); err != nil {
			serveError(w, http.StatusInternalServerError, err.Error())
		}
	})
}

func indexTmplExecute(w io.Writer, guest, host []profileEntry) error {
	var b bytes.Buffer
	b.WriteString(`<html>
<head>
<title>/debug/pprof</title>
<style>
.profile-name{
	display:inline-block;
	width:6rem;
}
</style>
</head>
<body>
/debug/pprof
<br>
<p>Set debug=1 as a query parameter to export in legacy text format (host only)</p>
<br>
Types of profiles available:
<table>
<thead><td>Count</td><td>Profile (guest)</td></thead>
`)

	for _, profile := range guest {
		link := &url.URL{Path: profile.Href}
		name := profile.Name
		fmt.Fprintf(&b, "<tr><td>%d</td><td><a href='%s'>%s</a></td></tr>\n", profile.Count, link, html.EscapeString(name))
	}

	b.WriteString(`</table>
<table>
<thead><td>Count</td><td>Profile (host)</td></thead>
`)

	for _, profile := range host {
		link := &url.URL{Path: profile.Href, RawQuery: fmt.Sprintf("host&debug=%d", profile.Debug)}
		name := profile.Name
		fmt.Fprintf(&b, "<tr><td>%d</td><td><a href='%s'>%s</a></td></tr>\n", profile.Count, link, html.EscapeString(name))
	}

	b.WriteString(`</table>
	<br>
	<p>
	Profile Descriptions:
	<ul>
	`)

	descriptionsByName := make(map[string]string, len(guest)+len(host))
	for _, profiles := range [][]profileEntry{guest, host} {
		for _, profile := range profiles {
			if profile.Desc != "" {
				descriptionsByName[profile.Name] = profile.Desc
			}
		}
	}
	descriptions := make([][2]string, 0, len(descriptionsByName))
	for name, desc := range descriptionsByName {
		descriptions = append(descriptions, [2]string{name, desc})
	}
	sort.Slice(descriptions, func(i, j int) bool {
		return descriptions[i][0] < descriptions[j][0]
	})
	for _, desc := range descriptions {
		fmt.Fprintf(&b, "<li><div class=profile-name>%s: </div> %s</li>\n", html.EscapeString(desc[0]), html.EscapeString(desc[1]))
	}
	b.WriteString(`</ul>
	</p>
	</body>
	</html>`)

	_, err := w.Write(b.Bytes())
	return err
}

var profileDescriptions = map[string]string{
	"allocs":       "A sampling of all past memory allocations",
	"block":        "Stack traces that led to blocking on synchronization primitives",
	"cmdline":      "The command line invocation of the current program",
	"goroutine":    "Stack traces of all current goroutines. Use debug=2 as a query parameter to export in the same format as an unrecovered panic.",
	"heap":         "A sampling of memory allocations of live objects. You can specify the gc GET parameter to run GC before taking the heap sample.",
	"mutex":        "Stack traces of holders of contended mutexes",
	"profile":      "CPU profile. You can specify the duration in the seconds GET parameter. After you get the profile file, use the go tool pprof command to investigate the profile.",
	"threadcreate": "Stack traces that led to the creation of new OS threads",
	"trace":        "A trace of execution of the current program. You can specify the duration in the seconds GET parameter. After you get the trace file, use the go tool trace command to investigate the trace.",
}
