package rudp

type opErr string

func (e opErr) Error() string   { return string(e) }
func (e opErr) Timeout() bool   { return string(e) == "timeout" }
func (e opErr) Temporary() bool { return false }

type closeErr string

func (e closeErr) Error() string   { return string(e) + " has been closed" }
func (e closeErr) Timeout() bool   { return false }
func (e closeErr) Temporary() bool { return false }
