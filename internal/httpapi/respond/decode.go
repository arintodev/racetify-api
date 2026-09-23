package respond

import (
	"encoding/json"
	"net/http"
)

// DecodeJSON decodes r's JSON body into dst, writing a standard 400
// invalid_request envelope (via Error) and returning false if decoding
// fails - the common case for a handler that has nothing else useful to
// do with a malformed body.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		Error(w, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
		return false
	}
	return true
}

// JSONDecode is DecodeJSON's variant for call sites (like the /oauth/token
// handler, which must speak both JSON and form-encoded bodies and shapes
// its own error response either way) that want the error returned instead
// of written to the response directly.
func JSONDecode(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}
