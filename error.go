package rudp

type opErr string

func (e opErr) Error() string   { return string(e) }
func (e opErr) Timeout() bool   { return string(e) == "timeout" }
func (e opErr) Temporary() bool { return false }

type timeoutErr string

func (e timeoutErr) Error() string   { return "timeout" }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return false }

type connCloseErr string

func (e connCloseErr) Error() string   { return "conn has been closed" }
func (e connCloseErr) Timeout() bool   { return false }
func (e connCloseErr) Temporary() bool { return false }

type rudpCloseErr string

func (e rudpCloseErr) Error() string   { return "rudp has been closed" }
func (e rudpCloseErr) Timeout() bool   { return false }
func (e rudpCloseErr) Temporary() bool { return false }
