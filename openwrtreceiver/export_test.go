package openwrtreceiver

var (
	ErrNotAvailable = errNotAvailable
	ErrNotFound     = errNotFound
)

// Runner runs the ubus tool.
type Runner = runner

// SetRunner makes the receivers created after it find ubus with f, until restore.
func SetRunner(f func(path string) (Runner, error)) (restore func()) {
	prev := newRunner
	newRunner = f
	return func() { newRunner = prev }
}
