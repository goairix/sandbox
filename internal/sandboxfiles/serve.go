package sandboxfiles

import (
	"encoding/json"
	"io"
)

func Serve(input io.Reader, output io.Writer) error { return serve(input, output, Run) }

func serve(input io.Reader, output io.Writer, run func(Request) Result) error {
	wire, err := io.ReadAll(io.LimitReader(input, MaxInputBytes+1))
	var req Request
	result := Result{}
	switch {
	case len(wire) > MaxInputBytes:
		result = failed("limit_exceeded")
	case err != nil || decodeStrict(wire, &req) != nil:
		result = failed("invalid_request")
	default:
		result = run(req)
	}
	wire, err = json.Marshal(result)
	if err != nil || len(wire)+1 > MaxOutputBytes {
		code := "limit_exceeded"
		if req.Operation == "edit" || req.Operation == "edit-lines" {
			code = "publish_uncertain"
		}
		wire, _ = json.Marshal(failed(code))
	}
	wire = append(wire, '\n')
	n, err := output.Write(wire)
	if err == nil && n != len(wire) {
		return io.ErrShortWrite
	}
	return err
}

func failed(code string) Result { return Result{Error: &Failure{Code: code}} }
