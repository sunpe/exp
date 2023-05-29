package slog

import (
	"context"
	"io"

	"golang.org/x/exp/slog/internal/buffer"
)

type SimpleHandler struct {
	*commonHandler
}

func NewSimpleHandler(w io.Writer, opts *HandlerOptions) *SimpleHandler {
	if opts == nil {
		opts = &HandlerOptions{}
	}
	return &SimpleHandler{
		commonHandler: &commonHandler{
			opts: *opts,
			w:    w,
		},
	}
}

func (h *SimpleHandler) Enabled(_ context.Context, level Level) bool {
	return h.commonHandler.enabled(level)
}

func (h *SimpleHandler) Handle(_ context.Context, record Record) error {
	state := h.newHandleState(buffer.New(), true, "", nil)
	defer state.free()

	// Built-in attributes. They are not in a group.
	stateGroups := state.groups
	state.groups = nil // So ReplaceAttrs sees no groups instead of the pre groups.
	// time
	if !record.Time.IsZero() {
		val := record.Time.Round(0) // strip monotonic to match Attr behavior
		state.appendNullKey()
		state.appendTime(val)
	}
	// level
	val := record.Level
	state.appendNullKey()
	state.appendString(val.String())

	state.groups = stateGroups // Restore groups passed to ReplaceAttrs.
	state.appendNonBuiltIns(record)

	// source
	if h.opts.AddSource {
		state.appendAttr(Any(SourceKey, record.source()))
	}
	msg := record.Message
	state.appendNullKey()
	state.appendString(msg)

	state.buf.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(*state.buf)
	return err
}

func (h *SimpleHandler) WithAttrs(attrs []Attr) Handler {
	return &SimpleHandler{commonHandler: h.commonHandler.withAttrs(attrs)}
}

func (h *SimpleHandler) WithGroup(name string) Handler {
	return &SimpleHandler{commonHandler: h.commonHandler.withGroup(name)}
}

var _ Handler = (*SimpleHandler)(nil)
