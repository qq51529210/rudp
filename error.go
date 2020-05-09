package rudp

import "errors"

type errOP string

func (e errOP) Error() string   { return string(e) }
func (e errOP) Timeout() bool   { return string(e) == "timeout" }
func (e errOP) Temporary() bool { return false }

var errClosed = errors.New("rudp has been closed")

