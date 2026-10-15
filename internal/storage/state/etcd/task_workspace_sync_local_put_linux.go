//go:build linux

package etcd

import "context"

func (s *PreparedTaskWorkspaceSyncSource) localCallComplete(stage string) {
	s.mu.Lock()
	completion := s.localIOComplete
	s.mu.Unlock()
	if completion != nil {
		completion(stage)
	}
}

func (s *PreparedTaskWorkspaceSyncSource) putLocal(ctx context.Context, body *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) (*taskWorkspaceSyncLocalWitness, error) {
	p, err := s.prepareLocalPut(ctx, body, target)
	if err != nil {
		return nil, err
	}
	if err := p.beginIO(); err != nil {
		p.release()
		return nil, err
	}
	defer p.finishIO()
	e, d := body.entry, p.draft.record
	if !validWorkspaceSyncPath(e.path) || e.path != d.RelativePath || e.mode != d.Mode || e.size != d.Size || e.offset != d.Offset || e.sha256 != d.BodySHA256 {
		return nil, ErrInvalidRecord
	}
	result, err := executeWorkspaceSyncLocalPut(p.ctx, target, workspaceSyncLocalFile{e.path, e.mode, e.size, e.offset, e.sha256}, body.readOwned, p.guard, s.localCallComplete)
	if err != nil {
		return nil, err
	}
	w := &taskWorkspaceSyncLocalWitness{origin: p, bytes: result.bytes, sha256: result.sha256, dev: result.dev, ino: result.ino}
	w.self, p.witness = w, w
	return w, nil
}
