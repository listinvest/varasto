package stoserver

import (
	"encoding/json"
	"fmt"
	"github.com/function61/gokit/sliceutil"
	"github.com/function61/varasto/pkg/stotypes"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"sync/atomic"
)

func panicIfError(err error) {
	if err != nil {
		panic(err)
	}
}

var bearerRe = regexp.MustCompile("^Bearer (.+)")

func authenticate(serverConfig *ServerConfig, w http.ResponseWriter, r *http.Request) bool {
	match := bearerRe.FindStringSubmatch(r.Header.Get("Authorization"))

	if match != nil {
		if _, tokenAllowed := serverConfig.ClientsAuthTokens[match[1]]; tokenAllowed {
			return true
		}
	}

	http.Error(w, "missing or incorrect Authorization header", http.StatusForbidden)
	return false
}

func outJson(w http.ResponseWriter, out interface{}) {
	w.Header().Set("Content-Type", "application/json")

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(out)
}

func collectionHasChangesetId(id string, coll *stotypes.Collection) bool {
	for _, changeset := range coll.Changesets {
		if changeset.ID == id {
			return true
		}
	}

	return false
}

func missingFromLeftHandSide(lhs []int, rhs []int) []int {
	missing := []int{}

	for _, item := range rhs {
		if !sliceutil.ContainsInt(lhs, item) {
			missing = append(missing, item)
		}
	}

	return missing
}

func contentTypeForFilename(path string) string {
	ext := filepath.Ext(path)

	// works with uppercase extensions as well
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return contentType
}

func parseStringBool(serialized string) (bool, error) {
	switch serialized {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf(`parseStringBool: expected "true" or "false"; got "%s"`, serialized)
	}
}

type nonBlockingLock struct {
	// same could be done with a buffered channel, but we'd need to do ..New()
	// and this design lets us benefit from zero value
	locked int32
}

func (b *nonBlockingLock) TryLock() (bool, func()) {
	if atomic.CompareAndSwapInt32(&b.locked, 0, 1) {
		return true, func() {
			if !atomic.CompareAndSwapInt32(&b.locked, 1, 0) {
				panic("we should have had exclusive access")
			}
		}
	} else {
		return false, func() { panic("should not be called") }
	}
}
