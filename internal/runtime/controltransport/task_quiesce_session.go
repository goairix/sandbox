package controltransport

import (
	"context"
	"io"
)

func (s *Session) SendTaskQuiesce(ctx context.Context, e TaskQuiesceEnvelope) error {
	return s.send(ctx, func(w io.Writer) error { return WriteTaskQuiesceRequest(w, e) })
}
