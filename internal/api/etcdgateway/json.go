package etcdgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/provisioning"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
)

const maxBody = 2 << 20

func readRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	wire, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if len(wire) > maxBody {
		writeError(w, 413, "body_too_large", "request body exceeds limit")
		return false
	}
	if err != nil || !utf8.Valid(wire) {
		writeError(w, 400, "invalid_request", "invalid JSON body")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.UseNumber()
	if strictJSON(d, reflect.TypeOf(output).Elem(), 0) != nil {
		writeError(w, 400, "invalid_request", "invalid JSON fields")
		return false
	}
	if _, err := d.Token(); err != io.EOF {
		writeError(w, 400, "invalid_request", "invalid JSON body")
		return false
	}
	d = json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(output); err != nil {
		writeError(w, 400, "invalid_request", "invalid JSON values")
		return false
	}
	return true
}

// Exact struct tags prevent encoding/json's case-insensitive aliases. Map keys
// (notably environment variable names) retain their original case and grammar.
func strictJSON(d *json.Decoder, shape reflect.Type, depth int) error {
	if depth > 32 {
		return errors.New("JSON depth")
	}
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		if shape.Kind() == reflect.Struct || token == nil {
			return errors.New("JSON value")
		}
		return nil
	}
	if delim == '[' {
		if shape.Kind() != reflect.Slice && shape.Kind() != reflect.Array {
			return errors.New("JSON array")
		}
		for d.More() {
			if err := strictJSON(d, shape.Elem(), depth+1); err != nil {
				return err
			}
		}
	} else if delim == '{' {
		if shape.Kind() != reflect.Map && shape.Kind() != reflect.Struct {
			return errors.New("JSON object")
		}
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return errors.New("JSON duplicate")
			}
			seen[name] = true
			var child reflect.Type
			if shape.Kind() == reflect.Map {
				child = shape.Elem()
			} else {
				for i := 0; i < shape.NumField(); i++ {
					field := shape.Field(i)
					if strings.Split(field.Tag.Get("json"), ",")[0] == name {
						child = field.Type
						break
					}
				}
			}
			if child == nil {
				return errors.New("JSON unknown field")
			}
			if err := strictJSON(d, child, depth+1); err != nil {
				return err
			}
		}
	} else {
		return errors.New("JSON container")
	}
	_, err = d.Token()
	return err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message})
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provisioning.ErrPlainColdNotFound):
		writeError(w, 404, "not_found", "sandbox not found")
	case errors.Is(err, state.ErrIdempotencyConflict):
		writeError(w, 409, "idempotency_conflict", "idempotency key conflicts with original request")
	case errors.Is(err, provisioning.ErrPlainColdProfile), errors.Is(err, state.ErrInvalidRecord):
		writeError(w, 400, "unsupported_profile", "unsupported request profile")
	case errors.Is(err, state.ErrPreparationCapacity), errors.Is(err, state.ErrSnapshotReadCapacity):
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, "capacity", "service capacity unavailable")
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, 503, "unavailable", "sandbox service unavailable")
	}
}
