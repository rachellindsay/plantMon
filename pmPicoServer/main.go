package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"machine"
	"net/netip"
	"strconv"
	"time"

	"github.com/soypat/cyw43439"
	"github.com/soypat/cyw43439/examples/common"
	"github.com/soypat/seqs/httpx"
	"github.com/soypat/seqs/stacks"
)

// constants that will be used to initialize the TCP stack
const (
	connTimeout = 3 * time.Second
	maxconns    = 3
	tcpbufsize  = 2030
	hostname    = "plantMon"
	listenPort  = 80
)

var (
	uart = machine.UART0
	tx   = machine.UART_TX_PIN
	rx   = machine.UART_RX_PIN
)

// new struct type that will convert values from plant monitor to json

type pmon struct {
	PMonW float64 `json:"wetness"`
	PMonT float64 `json:"temperature"`
	PMonH float64 `json:"humidity"`
}

// define package scoped variable of type pointer to slog.Logger that will serve as the default structured logger for the entire application
var logger *slog.Logger

// initialize default logger with Go init function, redirecting the output to the PicoW serial interface using machine.Serial type as first parameter - this will allow monitoring using 'tinygo monitor' command
func init() {
	logger = slog.New(
		slog.NewTextHandler(machine.Serial, &slog.HandlerOptions{
			// changed to LevelDebug from LevelInfo in order to get more information as it is not connecting
			Level: slog.LevelInfo,
		}))
}

// function to change PicoW LED state to show that device is active and listening for connections
func changeLEDState(dev *cyw43439.Device, state bool) {
	if err := dev.GPIOSet(0, state); err != nil {
		logger.Error("failed to change LED state:",
			slog.String("err", err.Error()))
	}
}

// use cyW43439 driver and the example code in examples/common from cyw43439 repo to define function that sets up the PicoW wifi
func setupDevice() (*stacks.PortStack, *cyw43439.Device) {
	logger.Info("Rae setupDevice here")
	_, stack, dev, err := common.SetupWithDHCP(common.SetupConfig{
		Hostname: hostname,
		Logger:   logger,
		TCPPorts: 1,
	})
	logger.Info("Rae setupDevice after SetupWithDHCP")

	if err != nil {
		panic("setup DHCP:" + err.Error())
	}
	// Turn LED on
	changeLEDState(dev, true)

	return stack, dev
}

// use the seqs/stack package to define function that listens on port 80 (as defined in const section near top) for incoming TCP connections
func newListener(stack *stacks.PortStack) *stacks.TCPListener {
	// start TCP server
	logger.Info("Rae newListener here")
	listenAddr := netip.AddrPortFrom(stack.Addr(), listenPort)
	listener, err := stacks.NewTCPListener(
		stack, stacks.TCPListenerConfig{
			MaxConnections: maxconns,
			ConnTxBufSize:  tcpbufsize,
			ConnRxBufSize:  tcpbufsize,
		})
	if err != nil {
		panic("listener create:" + err.Error())
	}
	err = listener.StartListening(listenPort)
	if err != nil {
		panic("listener start:" + err.Error())
	}
	logger.Info("listening",
		slog.String("addr", "http://"+listenAddr.String()),
	)
	return listener
}

// function to blink the LED in order to provide visual feedback when the PicoW server receives a connection request. it runs concurrently (using a Go channel) with other functions to avoid blocking the program
func blinkLED(dev *cyw43439.Device, blink chan uint) {
	logger.Info("Rae, blinkLED entered")
	for {
		select {
		case n := <-blink:
			logger.Info("Rae blinkLED %n from channel:", n)
			lastLEDState := true
			if n == 0 {
				n = 5
			}
			for i := uint(0); i < n; i++ {
				lastLEDState = !lastLEDState
				changeLEDState(dev, lastLEDState)
				time.Sleep(500 * time.Millisecond)
			}
			// ensure LED is on at the end
			changeLEDState(dev, true)
		}
	}
}

func readPmon(cmd byte) float64 {
	logger.Info("readPmon called",
		slog.Any("cmd", cmd))

	_, err := uart.Write([]byte{cmd, '\n'})

	if err != nil {
		panic("Error writing to UART")
	}
	// make is a built in function that allocates storage and initializes object buf. buf is a slice that can contain up to 128 bytes, but if more than 128 will only return 128.
	buf := make([]byte, 128)
	for {
		time.Sleep(100 * time.Millisecond)
		if uart.Buffered() > 0 {
			// n is the number of bytes that were read from the uart
			n, err := uart.Read(buf)
			if err != nil {
				logger.Info("Error reading from UART", slog.Any("err", err))
				panic("Error reading from UART")
			}
			logger.Info("pmon data",
				// buf[:n] is a slice from 0 - n (basically just the number of bytes read from the UART - should start with w= or t= or h=). buf[2:n-2] is a slice that omits the w/t/h= from the beginning of buf and the last two bytes (\r and \n - carriage return and new line) so that just the value w/t/h.
				slog.Any("buf", buf[:n]), slog.String("buf as number", string(buf[2:n-2])))
			returnValue, err := strconv.ParseFloat(string(buf[2:n-2]), 64)
			if err != nil {
				// pmon value should just be the value of w/t/h which should be a number
				panic("pmon value is not a number")
			}
			return returnValue
		}

	}
}

// function to obtain data readings(PMonW, PMonT, PMonH) from PM
func getPmon() *pmon {
	logger.Info("Getting from getPmon")
	currentW := readPmon('w')
	currentT := readPmon('t')
	currentH := readPmon('h')

	return &pmon{
		PMonW: currentW,
		PMonT: currentT,
		PMonH: currentH,
	}
}

func HTTPHandler(respWriter io.Writer, resp *httpx.ResponseHeader) {
	resp.SetConnectionClose()
	logger.Info("Got plant monitor data request...")
	pm := getPmon()

	body, err := json.Marshal(pm)
	if err != nil {
		logger.Error(
			"plant monitor json:",
			slog.String("err", err.Error()),
		)
		resp.SetStatusCode(500)
	} else {
		resp.SetContentType("application/json")
		resp.SetContentLength(len(body))
	}
	respWriter.Write(resp.Header())
	respWriter.Write(body)
}

// Function that handles HTTP connections and responds with temperature JSON. Define some buffer that can be reused for all connections to avoid memory allocations (because it's a small device). This function also takes a channel as an input. This channel notifies the blinkLED goroutine to blink the LED, showing that it is processing a request.
func handleConnection(listener *stacks.TCPListener, blink chan uint) {
	// Reuse the same buffers for each connection to avoid heap allocations
	var resp httpx.ResponseHeader
	logger.Info("Rae handleConnection start")
	buf := bufio.NewReaderSize(nil, 1024)

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error(
				"listener accept:",
				slog.String("err", err.Error()),
			)
			time.Sleep(time.Second)
			continue
		}

		logger.Info(
			"new connection",
			slog.String("remote",
				conn.RemoteAddr().String()),
		)
		err = conn.SetDeadline(time.Now().Add(connTimeout))
		if err != nil {
			conn.Close()
			logger.Error(
				"conn set deadline:",
				slog.String("err", err.Error()),
			)
			continue
		}
		buf.Reset(conn)
		resp.Reset()
		HTTPHandler(conn, &resp)
		conn.Close()

		blink <- 5

	}
}

// main function that serves the program. It sets up the wifi connection, creates the TCP listener, defines the blink channel to connect the blinkLED and handleConnections goroutines, and then starts both goroutines to process requests. It uses an infinite loop with a blocking select statement to prevent the program from terminating while the goroutines run in the background. It also sends a message every minute to the log to show that the program is running.
func main() {
	// time.Sleep seems to be needed in order for it to make the connection and it also gives time to get the monitor started.
	time.Sleep(time.Second * 15)
	logger.Info("Rae top of main")
	stack, dev := setupDevice()
	listener := newListener(stack)
	uart.Configure(machine.UARTConfig{BaudRate: 9600, TX: tx, RX: rx})

	blink := make(chan uint, 3)
	go blinkLED(dev, blink)
	go handleConnection(listener, blink)

	for {
		select {
		case <-time.After(1 * time.Minute):
			logger.Info("waiting for connections...")
		}
	}
}
