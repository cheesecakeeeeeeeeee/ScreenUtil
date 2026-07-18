package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// wl_display
	idDisplay = 1
	// Client owns everything from here
	idRegistry = 2
	idSync     = 3
	// These constants must be numbered in the exact order the objects are
	// created at runtime with no gaps
	idShm        = 4
	idOutput     = 5
	idScreencopy = 6
	idDataCtl    = 7
	idSeat       = 8
	idCompositor = 9
	idLayerShell = 10
	idFrame      = 11
	idPool       = 12
	idBuffer     = 13
	idKeyboard   = 14
	idSurface    = 15
	idLayerSurf  = 16
	idDataDev    = 17
	idSource     = 18
)

const (
	// zwlr_screencopy_frame_v1
	evBuffer     = 0
	evFlags      = 1
	evReady      = 2
	evFailed     = 3
	evBufferDone = 6
	// zwlr_data_control_source_v1
	evSend      = 0
	evCancelled = 1
	// zwlr_layer_surface_v1
	evLayerConfigure = 0
	evLayerClosed    = 1
	// wl_keyboard
	evKeymap = 0
	evKey    = 3
)

const (
	keyEnter        = 28
	keyKpEnter      = 96
	keyEscape       = 1
	keyStatePressed = 1
)

const (
	// wl_shm ARGB8888
	shmARGB = 0
	// wl_shm XRGB8888
	shmXRGB = 1
)

// ----------------------------------------------------------------------------
// Wire
// ----------------------------------------------------------------------------

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)

	return b
}

func i32(v int32) []byte {
	return u32(uint32(v))
}

// Encodes a Wayland string
// (length including NULL then bytes padded to 4)
func str(s string) []byte {
	l := len(s) + 1
	out := make([]byte, 4+((l+3)&^3))

	binary.LittleEndian.PutUint32(out, uint32(l))
	copy(out[4:], s)

	return out
}

// Encodes registry.bind
func newIDArg(iface string, ver, id uint32) []byte {
	return append(append(str(iface), u32(ver)...), u32(id)...)
}

func readU32(b []byte, off int) uint32 {
	return binary.LittleEndian.Uint32(b[off:])
}

func readStr(b []byte, off int) (string, int) {
	l := int(binary.LittleEndian.Uint32(b[off:]))
	// There's also a trailing NUL that we don't need
	s := string(b[off+4 : off+4+l-1])

	return s, off + 4 + ((l + 3) &^ 3)
}

// ----------------------------------------------------------------------------
// Connection
// ----------------------------------------------------------------------------

type conn struct {
	uc   *net.UnixConn
	rbuf []byte
	// These are recv via SCM_RIGHTS and consumed in order
	fds []int
}

func (c *conn) frame(obj, opcode uint32, args [][]byte) []byte {
	body := 0

	for _, a := range args {
		body += len(a)
	}

	size := 8 + body
	msg := make([]byte, 8, size)

	binary.LittleEndian.PutUint32(msg[0:], obj)
	binary.LittleEndian.PutUint32(msg[4:], uint32(size)<<16|opcode)

	for _, a := range args {
		msg = append(msg, a...)
	}

	return msg
}

func (c *conn) request(obj, opcode uint32, args ...[]byte) {
	if _, err := c.uc.Write(c.frame(obj, opcode, args)); err != nil {
		slog.Error("Write request", "err", err)
		os.Exit(1)
	}
}

func (c *conn) requestFD(obj, opcode uint32, fd int, args ...[]byte) {
	oob := syscall.UnixRights(fd)

	if _, _, err := c.uc.WriteMsgUnix(c.frame(obj, opcode, args), oob, nil); err != nil {
		slog.Error("Write request with fd", "err", err)
		os.Exit(1)
	}
}

func (c *conn) fill() {
	tmp := make([]byte, 1<<16)
	oob := make([]byte, 256)

	n, oobn, _, _, err := c.uc.ReadMsgUnix(tmp, oob)
	if err != nil {
		slog.Error("Read from socket", "err", err)
		os.Exit(1)
	}

	c.rbuf = append(c.rbuf, tmp[:n]...)
	if oobn > 0 {
		scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			slog.Error("Parse socket control message", "err", err)
			os.Exit(1)
		}

		for _, scm := range scms {
			fds, err := syscall.ParseUnixRights(&scm)
			if err == nil {
				c.fds = append(c.fds, fds...)
			}
		}
	}
}

// Returns the next complete message
func (c *conn) read() (uint32, uint32, []byte) {
	for len(c.rbuf) < 8 {
		c.fill()
	}

	obj := binary.LittleEndian.Uint32(c.rbuf[0:])
	word := binary.LittleEndian.Uint32(c.rbuf[4:])
	size, op := int(word>>16), word&0xffff

	for len(c.rbuf) < size {
		c.fill()
	}

	body := append([]byte(nil), c.rbuf[8:size]...)
	c.rbuf = append([]byte(nil), c.rbuf[size:]...)

	if obj == idDisplay && op == 0 { // wl_display.error
		eObj, code := readU32(body, 0), readU32(body, 4)
		msg, _ := readStr(body, 8)

		slog.Error("Wayland protocol error", "object", eObj, "code", code, "msg", msg)
		os.Exit(1)
	}

	if obj == idKeyboard && op == evKeymap {
		c.dropHeadFD()
	}

	return obj, op, body
}

// ----------------------------------------------------------------------------
// Capture
// ----------------------------------------------------------------------------

func (c *conn) captureParams(scVer uint32) (format, w, h, stride uint32) {
	for {
		id, op, body := c.read()

		if id == idFrame {
			switch op {
			case evBuffer:
				if f := readU32(body, 0); f == shmARGB || f == shmXRGB {
					format, w, h, stride = f, readU32(body, 4), readU32(body, 8), readU32(body, 12)
				}
			case evBufferDone:
				if stride == 0 {
					slog.Error("Compositor offered no supported shm buffer format")
					os.Exit(1)
				}
				return
			case evFailed:
				slog.Error("Compositor failed to describe the frame")
				os.Exit(1)
			}
		}

		if scVer < 3 && stride != 0 { // these have no buffer_done
			return
		}
	}
}

func (c *conn) waitReady() (yInvert bool) {
	for {
		id, op, body := c.read()

		if id == idFrame {
			switch op {
			case evFlags:
				yInvert = readU32(body, 0)&1 != 0
			case evReady:
				return
			case evFailed:
				slog.Error("Compositor failed to copy the frame")
				os.Exit(1)
			}
		}
	}
}

func (c *conn) waitKey() bool {
	for {
		id, op, body := c.read()

		if id != idKeyboard {
			continue
		}

		switch op {
		case evKey:
			// key(serial, time, key, state)
			key := readU32(body, 8)
			state := readU32(body, 12)
			if state != keyStatePressed {
				continue
			}
			switch key {
			case keyEnter, keyKpEnter:
				return true
			case keyEscape:
				return false
			}
		}
	}
}

func (c *conn) dropHeadFD() {
	if len(c.fds) > 0 {
		syscall.Close(c.fds[0])
		c.fds = c.fds[1:]
	}
}

func main() {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		slog.Error("xDG_RUNTIME_DIR not set")
		os.Exit(1)
	}

	disp := os.Getenv("WAYLAND_DISPLAY")
	if disp == "" {
		disp = "wayland-0"
	}

	sock := disp
	if !filepath.IsAbs(sock) {
		sock = filepath.Join(runtime, disp)
	}

	uc, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		slog.Error("Connect to wayland socket", "socket", sock, "err", err)
		os.Exit(1)
	}
	defer uc.Close()
	c := &conn{uc: uc}

	c.request(idDisplay, 1, u32(idRegistry)) // wl_display.get_registry
	c.request(idDisplay, 0, u32(idSync))     // wl_display.sync
	globals := map[string][2]uint32{}

	for {
		id, op, body := c.read()

		if id == idRegistry && op == 0 { // global
			name := readU32(body, 0)
			iface, off := readStr(body, 4)
			ver := readU32(body, off)

			if _, seen := globals[iface]; !seen {
				globals[iface] = [2]uint32{name, ver}
			}
		} else if id == idSync && op == 0 { // callback.done
			break
		}
	}

	bind := func(iface string, newID, cap uint32) uint32 {
		global, ok := globals[iface]

		if !ok {
			slog.Error("Required wayland global missing", "interface", iface)
			os.Exit(1)
		}

		ver := min(global[1], cap)
		c.request(idRegistry, 0, u32(global[0]), newIDArg(iface, ver, newID))

		return ver
	}
	bind("wl_shm", idShm, 1)
	bind("wl_output", idOutput, 1)
	scVer := bind("zwlr_screencopy_manager_v1", idScreencopy, 3)
	bind("ext_data_control_manager_v1", idDataCtl, 1)
	bind("wl_seat", idSeat, 1)
	bind("wl_compositor", idCompositor, 4)
	bind("zwlr_layer_shell_v1", idLayerShell, 1)

	// Capture the first output
	c.request(idScreencopy, 0, u32(idFrame), u32(0), u32(idOutput))
	format, w, h, stride := c.captureParams(scVer)

	// This is the shared memroy buffer the compositor writes to
	size := int(stride * h)
	f, err := os.CreateTemp(runtime, "screenutil-*")
	if err != nil {
		slog.Error("Create shm temp file", "err", err)
		os.Exit(1)
	}
	os.Remove(f.Name()) // fd keeps it alive anyways

	if err := f.Truncate(int64(size)); err != nil {
		slog.Error("Size shm file", "err", err)
		os.Exit(1)
	}

	mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		slog.Error("Mmap shm file", "err", err)
		os.Exit(1)
	}

	c.requestFD(idShm, 0, int(f.Fd()), u32(idPool), u32(uint32(size)))
	c.request(idPool, 0, u32(idBuffer), u32(0), u32(w), u32(h), u32(stride), u32(format))
	c.request(idFrame, 0, u32(idBuffer))
	yInvert := c.waitReady()

	img := image.NewRGBA(image.Rect(0, 0, int(w), int(h)))
	for y := 0; y < int(h); y++ {
		src := y

		if yInvert {
			src = int(h) - 1 - y
		}

		row := mem[src*int(stride):]

		for x := 0; x < int(w); x++ {
			o := x * 4
			a := byte(255)

			if format == shmARGB {
				a = row[o+3]
			}

			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = row[o+2], row[o+1], row[o], a

		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		slog.Error("Encode PNG", "err", err)
		os.Exit(1)
	}
	pngBytes := buf.Bytes()

	if yInvert {
		tmp := make([]byte, stride)
		for y := 0; y < int(h)/2; y++ {
			top := mem[y*int(stride) : (y+1)*int(stride)]
			bot := mem[(int(h)-1-y)*int(stride) : (int(h)-y)*int(stride)]
			copy(tmp, top)
			copy(top, bot)
			copy(bot, tmp)
		}
	}

	c.request(idSeat, 1, u32(idKeyboard)) // wl_seat.get_keyboard

	c.request(idCompositor, 0, u32(idSurface)) // wl_compositor.create_surface

	// get_layer_surface(id, surface, output=null, layer=overlay, namespace)
	c.request(idLayerShell, 0, u32(idLayerSurf), u32(idSurface), u32(0), u32(3), str("screenutil"))

	c.request(idLayerSurf, 1, u32(1|2|4|8)) // set_anchor(top|bottom|left|right)
	c.request(idLayerSurf, 2, i32(-1))      // set_exclusive_zone(-1): ignore other panels
	c.request(idLayerSurf, 4, u32(1))       // set_keyboard_interactivity(exclusive)
	c.request(idSurface, 6)                 // wl_surface.commit (triggers configure)

	for configured := false; !configured; {
		id, op, body := c.read()
		if id == idLayerSurf && op == evLayerConfigure {
			serial := readU32(body, 0)
			c.request(idLayerSurf, 6, u32(serial))                 // ack_configure
			c.request(idSurface, 1, u32(idBuffer), u32(0), u32(0)) // attach
			c.request(idSurface, 6)                                // commit
			configured = true
		}
		if id == idLayerSurf && op == evLayerClosed {
			slog.Error("Compositor closed the freeze overlay")
			os.Exit(1)
		}
	}

	confirmed := c.waitKey()

	c.request(idLayerSurf, 7) // zwlr_layer_surface_v1.destroy
	c.request(idSurface, 0)   // wl_surface.destroy
	syscall.Munmap(mem)
	f.Close()

	if !confirmed {
		slog.Info("cancelled")
		return
	}

	c.request(idDataCtl, 1, u32(idDataDev), u32(idSeat)) // get_data_device
	c.request(idDataCtl, 0, u32(idSource))               // create_data_source
	c.request(idSource, 0, str("image/png"))             // source.offer
	c.request(idDataDev, 0, u32(idSource))               // device.set_selection

	for {
		id, op, _ := c.read()

		if id == idSource {
			switch op {
			case evSend:
				if len(c.fds) == 0 {
					continue
				}

				fd := c.fds[0]
				c.fds = c.fds[1:]

				out := os.NewFile(uintptr(fd), "clip")
				n, err := out.Write(pngBytes)
				out.Close()
				slog.Info("Served clipboard data", "bytes", n, "err", err)
			case evCancelled:
				slog.Warn("Data source cancelled by compositor")
				return
			}
		}
	}
}
