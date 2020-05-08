package rudp

type errDial string

func (e errDial) Error() string   { return string(e) }
func (e errDial) Timeout() bool   { return false }
func (e errDial) Temporary() bool { return false }

type errClosed string

func (e errClosed) Error() string   { return string(e) + " has been closed" }
func (e errClosed) Timeout() bool   { return false }
func (e errClosed) Temporary() bool { return false }

type errTimeout string

func (e errTimeout) Error() string   { return string(e) + " timeout" }
func (e errTimeout) Timeout() bool   { return true }
func (e errTimeout) Temporary() bool { return false }
