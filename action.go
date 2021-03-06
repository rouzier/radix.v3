package radix

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mediocregopher/radix.v3/resp"
)

// Action can perform one or more tasks using a Conn
type Action interface {
	// Keys returns the keys which will be acted on. Empty slice or nil may be
	// returned if no keys are being acted on.
	Keys() []string

	// Run actually performs the Action using the given Conn
	Run(c Conn) error
}

// CmdAction is a specific type of Action for which a command is marshaled and
// sent to the server and the command's response is read and unmarshaled into a
// receiver within the CmdAction.
//
// A CmdAction can be used like an Action, but it can also be used by marshaling
// the command and unmarshaling the response manually.
type CmdAction interface {
	Action
	resp.Marshaler
	resp.Unmarshaler
}

var noKeyCmds = map[string]bool{
	"SENTINEL": true,

	"CLUSTER":   true,
	"READONLY":  true,
	"READWRITE": true,
	"ASKING":    true,

	"AUTH":   true,
	"ECHO":   true,
	"PING":   true,
	"QUIT":   true,
	"SELECT": true,
	"SWAPDB": true,

	"KEYS":      true,
	"MIGRATE":   true,
	"OBJECT":    true,
	"RANDOMKEY": true,
	"WAIT":      true,
	"SCAN":      true,

	"EVAL":    true,
	"EVALSHA": true,
	"SCRIPT":  true,

	"BGREWRITEAOF": true,
	"BGSAVE":       true,
	"CLIENT":       true,
	"COMMAND":      true,
	"CONFIG":       true,
	"DBSIZE":       true,
	"DEBUG":        true,
	"FLUSHALL":     true,
	"FLUSHDB":      true,
	"INFO":         true,
	"LASTSAVE":     true,
	"MONITOR":      true,
	"ROLE":         true,
	"SAVE":         true,
	"SHUTDOWN":     true,
	"SLAVEOF":      true,
	"SLOWLOG":      true,
	"SYNC":         true,
	"TIME":         true,

	"DISCARD": true,
	"EXEC":    true,
	"MULTI":   true,
	"UNWATCH": true,
	"WATCH":   true,
}

func cmdString(m resp.Marshaler) string {
	// we go way out of the way here to display the command as it would be sent
	// to redis. This is pretty similar logic to what the stub does as well
	buf := new(bytes.Buffer)
	if err := m.MarshalRESP(buf); err != nil {
		return fmt.Sprintf("error creating string: %q", err.Error())
	}
	var ss []string
	err := resp.RawMessage(buf.Bytes()).UnmarshalInto(resp.Any{I: &ss})
	if err != nil {
		return fmt.Sprintf("error creating string: %q", err.Error())
	}
	for i := range ss {
		ss[i] = strconv.QuoteToASCII(ss[i])
	}
	return "[" + strings.Join(ss, " ") + "]"
}

func marshalBulkString(prevErr error, w io.Writer, str string) error {
	if prevErr != nil {
		return prevErr
	}
	return resp.BulkString{S: str}.MarshalRESP(w)
}

func marshalBulkStringBytes(prevErr error, w io.Writer, b []byte) error {
	if prevErr != nil {
		return prevErr
	}
	return resp.BulkStringBytes{B: b}.MarshalRESP(w)
}

////////////////////////////////////////////////////////////////////////////////

type cmdAction struct {
	rcv  interface{}
	cmd  string
	args []string
}

// Cmd is used to perform a redis command and retrieve a result. See the package
// docs on how results are unmarshaled into the receiver.
//
//	if err := client.Do(radix.Cmd(nil, "SET", "foo", "bar")); err != nil {
//		panic(err)
//	}
//
//	var fooVal string
//	if err := client.Do(radix.Cmd(&fooVal, "GET", "foo")); err != nil {
//		panic(err)
//	}
//	fmt.Println(fooVal) // "bar"
//
// If the receiver value of Cmd is a primitive or slice/map a pointer must be
// passed in. It may also be an io.Writer, an encoding.Text/BinaryUnmarshaler,
// or a resp.Unmarshaler.
func Cmd(rcv interface{}, cmd string, args ...string) CmdAction {
	return &cmdAction{
		rcv:  rcv,
		cmd:  cmd,
		args: args,
	}
}

func (c *cmdAction) Keys() []string {
	cmd := strings.ToUpper(c.cmd)
	if cmd == "BITOP" && len(c.args) > 1 { // antirez why you do this
		return c.args[1:]
	} else if noKeyCmds[cmd] || len(c.args) == 0 {
		return nil
	}
	return []string{c.args[0]}
}

func (c *cmdAction) MarshalRESP(w io.Writer) error {
	err := resp.ArrayHeader{N: len(c.args) + 1}.MarshalRESP(w)
	err = marshalBulkString(err, w, c.cmd)
	for i := range c.args {
		err = marshalBulkString(err, w, c.args[i])
	}
	return err
}

func (c *cmdAction) UnmarshalRESP(br *bufio.Reader) error {
	return resp.Any{I: c.rcv}.UnmarshalRESP(br)
}

func (c *cmdAction) Run(conn Conn) error {
	if err := conn.Encode(c); err != nil {
		return err
	}
	return conn.Decode(c)
}

func (c *cmdAction) String() string {
	return cmdString(c)
}

////////////////////////////////////////////////////////////////////////////////

type flatCmdAction struct {
	rcv      interface{}
	cmd, key string
	args     []interface{}
}

// FlatCmd is like Cmd, but the arguments can be of almost any type, and FlatCmd
// will automatically flatten them into a single array of strings.
//
// FlatCmd does _not_ work for commands whose first parameter isn't a key. Use
// Cmd for those.
//
//	client.Do(radix.FlatCmd(nil, "SET", "foo", 1))
//	// performs "SET" "foo" "1"
//
//	client.Do(radix.FlatCmd(nil, "SADD", "fooSet", []string{"1", "2", "3"}))
//	// performs "SADD" "fooSet" "1" "2" "3"
//
//	m := map[string]int{"a":1, "b":2, "c":3}
//	client.Do(radix.FlatCmd(nil, "HMSET", "fooHash", m))
//	// performs "HMSET" "foohash" "a" "1" "b" "2" "c" "3"
//
//	// FlatCmd also supports using a resp.LenReader (an io.Reader with a Len()
//	// method) as an argument. *bytes.Buffer is an example of a LenReader,
//	// and the resp package has a NewLenReader function which can wrap an
//	// existing io.Reader. For example, if writing an http.Request body:
//	bl := resp.NewLenReader(req.Body, req.ContentLength)
//	client.Do(radix.FlatCmd(nil, "SET", "fooReq", bl))
//
// FlatCmd also supports encoding.Text/BinaryMarshalers. It does _not_ currently
// support resp.Marshaler.
//
// The receiver to FlatCmd follows the same rules as for Cmd.
func FlatCmd(rcv interface{}, cmd, key string, args ...interface{}) CmdAction {
	return &flatCmdAction{
		rcv:  rcv,
		cmd:  cmd,
		key:  key,
		args: args,
	}
}

func (c *flatCmdAction) Keys() []string {
	return []string{c.key}
}

func (c *flatCmdAction) MarshalRESP(w io.Writer) error {
	var err error
	a := resp.Any{
		I:                     c.args,
		MarshalBulkString:     true,
		MarshalNoArrayHeaders: true,
	}
	arrL := 2 + a.NumElems()
	err = resp.ArrayHeader{N: arrL}.MarshalRESP(w)
	err = marshalBulkString(err, w, c.cmd)
	err = marshalBulkString(err, w, c.key)
	if err != nil {
		return err
	}
	return a.MarshalRESP(w)
}

func (c *flatCmdAction) UnmarshalRESP(br *bufio.Reader) error {
	return resp.Any{I: c.rcv}.UnmarshalRESP(br)
}

func (c *flatCmdAction) Run(conn Conn) error {
	if err := conn.Encode(c); err != nil {
		return err
	}
	return conn.Decode(c)
}

func (c *flatCmdAction) String() string {
	return cmdString(c)
}

////////////////////////////////////////////////////////////////////////////////

// EvalScript contains the body of a script to be used with redis' EVAL
// functionality. Call Cmd on a EvalScript to actually create an Action which
// can be run.
//
//	var getSet = NewEvalScript(1, `
//		local prev = redis.call("GET", KEYS[1])
//		redis.call("SET", KEYS[1], ARGV[1])
//		return prev
//	`)
//
//	func doAThing(c radix.Client) (string, error) {
//		var prevVal string
//		err := c.Do(getSet.Cmd(&string, "myKey", "myVal"))
//		return prevVal, err
//	}
//
type EvalScript struct {
	script, sum string
	numKeys     int
}

// NewEvalScript initializes a EvalScript instance. numKeys corresponds to the
// number of arguments which will be keys when Cmd is called
func NewEvalScript(numKeys int, script string) EvalScript {
	sumRaw := sha1.Sum([]byte(script))
	sum := hex.EncodeToString(sumRaw[:])
	return EvalScript{
		script:  script,
		sum:     sum,
		numKeys: numKeys,
	}
}

var (
	evalsha = []byte("EVALSHA")
	eval    = []byte("EVAL")
)

type evalAction struct {
	EvalScript
	args []string
	rcv  interface{}

	eval bool
}

// Cmd is like the top-level Cmd but it uses the the EvalScript to perform an
// EVALSHA command (and will automatically fallback to EVAL as necessary). args
// must be at least as long as the numKeys argument of NewEvalScript.
func (es EvalScript) Cmd(rcv interface{}, args ...string) Action {
	if len(args) < es.numKeys {
		panic("not enough arguments passed into EvalScript.Cmd")
	}
	return &evalAction{
		EvalScript: es,
		args:       args,
		rcv:        rcv,
	}
}

func (ec *evalAction) Keys() []string {
	return ec.args[:ec.numKeys]
}

func (ec *evalAction) MarshalRESP(w io.Writer) error {
	// EVAL(SHA) script/sum numkeys args...
	if err := (resp.ArrayHeader{N: 3 + len(ec.args)}).MarshalRESP(w); err != nil {
		return err
	}

	var err error
	if ec.eval {
		err = marshalBulkStringBytes(err, w, eval)
		err = marshalBulkString(err, w, ec.script)
	} else {
		err = marshalBulkStringBytes(err, w, evalsha)
		err = marshalBulkString(err, w, ec.sum)
	}

	err = marshalBulkString(err, w, strconv.Itoa(ec.numKeys))
	for i := range ec.args {
		err = marshalBulkString(err, w, ec.args[i])
	}
	return err
}

func (ec *evalAction) Run(conn Conn) error {
	run := func(eval bool) error {
		ec.eval = eval
		if err := conn.Encode(ec); err != nil {
			return err
		}
		return conn.Decode(resp.Any{I: ec.rcv})
	}

	err := run(false)
	if err != nil && strings.HasPrefix(err.Error(), "NOSCRIPT") {
		err = run(true)
	}
	return err
}

////////////////////////////////////////////////////////////////////////////////

type pipeline []CmdAction

// Pipeline returns an Action which first writes multiple commands to a Conn in
// a single write, then reads their responses in a single read. This reduces
// network delay into a single round-trip.
//
//	var fooVal string
//	p := radix.Pipeline(
//		radix.FlatCmd(nil, "SET", "foo", 1),
//		radix.Cmd(&fooVal, "GET", "foo"),
//	)
//	if err := conn.Do(p); err != nil {
//		panic(err)
//	}
//	fmt.Printf("fooVal: %q\n", fooVal)
//
func Pipeline(cmds ...CmdAction) Action {
	return pipeline(cmds)
}

func (p pipeline) Keys() []string {
	m := map[string]bool{}
	for _, rc := range p {
		for _, k := range rc.Keys() {
			m[k] = true
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func (p pipeline) Run(c Conn) error {
	for _, cmd := range p {
		if err := c.Encode(cmd); err != nil {
			return err
		}
	}
	for _, cmd := range p {
		if err := c.Decode(cmd); err != nil {
			return err
		}
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////

type withConn struct {
	key string
	fn  func(Conn) error
}

// WithConn is used to perform a set of independent Actions on the same Conn.
// key should be a key which one or more of the inner Actions is acting on, or
// "" if no keys are being acted on. The callback function is what should
// actually carry out the inner actions, and the error it returns will be
// passed back up immediately.
//
//	err := pool.Do(WithConn("someKey", func(conn Conn) error {
//		var curr int
//		if err := conn.Do(radix.Cmd(&curr, "GET", "someKey")); err != nil {
//			return err
//		}
//
//		curr++
//		return conn.Do(radix.Cmd(nil, "SET", "someKey", curr))
//	})
//
// NOTE that WithConn only ensures all inner Actions are performed on the same
// Conn, it doesn't make them transactional. Use MULTI/WATCH/EXEC within a
// WithConn or Pipeline for transactions, or use EvalScript
func WithConn(key string, fn func(Conn) error) Action {
	return &withConn{key, fn}
}

func (wc *withConn) Keys() []string {
	return []string{wc.key}
}

func (wc *withConn) Run(c Conn) error {
	return wc.fn(c)
}
