package fetch_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/fetch"
)

func cachedPin(url string, data []byte) catalog.ArchivePin {
	return catalog.ArchivePin{Version: "1.0", URL: url + "/tool.tar.gz", SHA256: sha256Hex(data), Format: "tar.gz", Commands: map[string]string{"tool": "tool"}, BinDirs: []string{"."}}
}

func cacheEntries(t *testing.T, cache string) []string {
	t.Helper()
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestDownloadCacheResumesInTheNextRun(t *testing.T) {
	data := archiveTar(t, []archiveFixtureEntry{{Name: "tool", Text: string(transferData(256 << 10)), Mode: 0755}})
	half := len(data) / 2
	server, log := transferServer(t, false, func(attempt int, w http.ResponseWriter, r *http.Request) {
		switch attempt {
		case 1:
			serveRanges(&cutAfter{ResponseWriter: w, left: half}, r, data)
		case 2:
			w.WriteHeader(http.StatusForbidden)
		default:
			serveRanges(w, r, data)
		}
	})
	pin := cachedPin(server.URL, data)
	base := t.TempDir()
	cache := filepath.Join(base, "downloads")
	destination := filepath.Join(base, "payload")
	withoutSlowWindow := testArchiveTransfer
	withoutSlowWindow.Window = withoutSlowWindow.Limit
	if err := fetch.Download(t.Context(), pin, destination, fetch.Options{Client: server.Client(), Cache: cache, Transfer: withoutSlowWindow}); err == nil {
		t.Fatal("the cut transfer succeeded")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unfinished download reached extraction", err)
	}
	info, err := os.Stat(filepath.Join(cache, pin.SHA256+".part"))
	if err != nil || info.Size() != int64(half) {
		t.Fatalf("kept download = %v, %v; want %d bytes", info, err, half)
	}
	if err := fetch.Download(t.Context(), pin, destination, fetch.Options{Client: server.Client(), Cache: cache, Transfer: withoutSlowWindow}); err != nil {
		t.Fatal(err)
	}
	requests := log.all()
	if len(requests) != 3 || requests[2].Header.Get("Range") != "bytes="+strconv.Itoa(half)+"-" || requests[2].Header.Get("If-Range") != `"fixture"` {
		t.Fatalf("requests = %d, resume Range %q, If-Range %q", len(requests), requests[len(requests)-1].Header.Get("Range"), requests[len(requests)-1].Header.Get("If-Range"))
	}
	if _, err := os.Stat(filepath.Join(destination, "tool")); err != nil {
		t.Fatal(err)
	}
	if names := cacheEntries(t, cache); len(names) != 0 {
		t.Fatal("an extracted download stayed in the cache", names)
	}
}

func TestDownloadCacheRestartsWhenTheKeptBytesAreNotThePinnedFile(t *testing.T) {
	data := archiveTar(t, []archiveFixtureEntry{{Name: "tool", Text: "program", Mode: 0755}})
	for _, test := range []struct{ name, record string }{
		{"no record", ""},
		{"other URL", `{"url":"https://example.invalid/tool.tar.gz","total":1000000}`},
		{"invalid record", `{"url":`},
		{"changed file on the server", "resume"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, log := transferServer(t, false, func(_ int, w http.ResponseWriter, r *http.Request) {
				serveRanges(w, r, data)
			})
			pin := cachedPin(server.URL, data)
			base := t.TempDir()
			cache := filepath.Join(base, "downloads")
			if err := os.Mkdir(cache, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cache, pin.SHA256+".part"), []byte("other bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			record := test.record
			if record == "resume" {
				record = `{"url":"` + pin.URL + `","total":` + strconv.Itoa(len(data)) + `,"validator":"\"old\""}`
			}
			if record != "" {
				if err := os.WriteFile(filepath.Join(cache, pin.SHA256+".json"), []byte(record), 0600); err != nil {
					t.Fatal(err)
				}
			}
			destination := filepath.Join(base, "payload")
			if err := fetch.Download(t.Context(), pin, destination, fetch.Options{Client: server.Client(), Cache: cache, Transfer: testArchiveTransfer}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(destination, "tool"))
			if err != nil || string(got) != "program" {
				t.Fatal("extracted command differs", string(got), err)
			}
			requests := log.all()
			if resumed := requests[0].Header.Get("Range") != ""; resumed != (test.record == "resume") {
				t.Fatalf("first request Range = %q", requests[0].Header.Get("Range"))
			}
		})
	}
}

func TestDownloadCacheDropsBytesThatFailThePin(t *testing.T) {
	data := archiveTar(t, []archiveFixtureEntry{{Name: "tool", Text: "program", Mode: 0755}})
	server, _ := transferServer(t, false, func(_ int, w http.ResponseWriter, r *http.Request) {
		serveRanges(w, r, data)
	})
	pin := cachedPin(server.URL, data)
	pin.SHA256 = sha256Hex([]byte("another file"))
	base := t.TempDir()
	cache := filepath.Join(base, "downloads")
	destination := filepath.Join(base, "payload")
	if err := fetch.Download(t.Context(), pin, destination, fetch.Options{Client: server.Client(), Cache: cache}); err == nil {
		t.Fatal("accepted incorrect checksum")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("extracted bytes before checksum verification", err)
	}
	if names := cacheEntries(t, cache); len(names) != 0 {
		t.Fatal("bytes that fail the pin stayed in the cache", names)
	}
}

func TestDownloadCacheReusesAVerifiedDownloadWithoutTheNetwork(t *testing.T) {
	data := archiveTar(t, []archiveFixtureEntry{{Name: "other", Text: "program", Mode: 0755}})
	server, log := transferServer(t, false, func(_ int, w http.ResponseWriter, r *http.Request) {
		serveRanges(w, r, data)
	})
	pin := cachedPin(server.URL, data)
	base := t.TempDir()
	cache := filepath.Join(base, "downloads")
	if err := fetch.Download(t.Context(), pin, filepath.Join(base, "first"), fetch.Options{Client: server.Client(), Cache: cache}); err == nil {
		t.Fatal("accepted a payload without its command")
	}
	pin.Commands = map[string]string{"other": "other"}
	if err := fetch.Download(t.Context(), pin, filepath.Join(base, "second"), fetch.Options{Client: server.Client(), Cache: cache}); err != nil {
		t.Fatal(err)
	}
	if len(log.all()) != 1 {
		t.Fatalf("requests = %d, want one download for two extractions", len(log.all()))
	}
}

func TestTransferAcceptsOnlyTheRequestedRemainder(t *testing.T) {
	data := transferData(128 << 10)
	size := strconv.Itoa(len(data))
	for value, want := range map[string]bool{
		"bytes 4096-" + strconv.Itoa(len(data)-1) + "/" + size:                      true,
		"bytes 4096-" + strconv.Itoa(len(data)-2) + "/" + size:                      false,
		"bytes 4095-" + strconv.Itoa(len(data)-1) + "/" + size:                      false,
		"bytes 4096-" + strconv.Itoa(len(data)-1) + "/*":                            false,
		"bytes 4096-" + strconv.Itoa(len(data)-1) + "/" + strconv.Itoa(len(data)+1): false,
		"items 4096-" + strconv.Itoa(len(data)-1) + "/" + size:                      false,
		"bytes 4096-" + strconv.Itoa(len(data)-1):                                   false,
		"": false,
	} {
		t.Run(value, func(t *testing.T) {
			server, log := transferServer(t, false, func(attempt int, w http.ResponseWriter, r *http.Request) {
				switch attempt {
				case 1:
					serveRanges(&cutAfter{ResponseWriter: w, left: 4096}, r, data)
				case 2:
					if value != "" {
						w.Header().Set("Content-Range", value)
					}
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(data[4096:])
				default:
					serveRanges(w, r, data)
				}
			})
			written, err := fetchForTest(t, testArchiveTransfer, server.Client(), server.URL+"/tool", nil)
			if err != nil || string(written) != string(data) {
				t.Fatal("the transfer did not give the complete file", err)
			}
			if accepted := len(log.all()) == 2; accepted != want {
				t.Fatalf("requests = %d for Content-Range %q", len(log.all()), value)
			}
		})
	}
}
