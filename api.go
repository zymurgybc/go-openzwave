package openzwave

//
// Provides a facade for the C++ API that exposes just enough of the underlying C++
// API to be useful to implementing the Ninja Zwave driver
//
// The functions in this module are responsible for marshalling to and from the C functions
// declared in api.hpp and api.cpp.
//

//
// The following #cgo directives assumed that 'go' is a symbolic link that references the gopath that contains the current directory, e.g. ../../../..
//
// All 'go' packages that have this package as a dependency should include such a go link and will then inherit the library built in this package.
//

// #cgo LDFLAGS: -lopenzwave -Lgo/src/github.com/ninjasphere/go-openzwave/openzwave
// #cgo CPPFLAGS: -Iopenzwave/cpp/src/platform -Iopenzwave/cpp/src -Iopenzwave/cpp/src/value_classes
//
// #include "api.h"
import "C"

import (
	"fmt"
	"os"
	"os/signal"
	"time"
	"unsafe"

	"github.com/ninjasphere/go-openzwave/CODE"
	"github.com/ninjasphere/go-openzwave/NT"
	"github.com/ninjasphere/go-openzwave/VT"
)

type api struct {
	options       C.Options // an opaque reference to C++ Options object
	notifications chan Notification
	device        string
	quit          chan bool
	manager       C.Manager
}

type API interface {
	// notifications are received on this channel
	Notifications() chan Notification

	// free a notification after use.
	FreeNotification(Notification)

	//
	// Used to tell the event loop to quit.
	//
	QuitSignal() chan bool
}

//
// The Phase0 -> Phase2 interface represent 3 different states in the evolution of the api from
// creation, through configuration, through use.
//
// Each phase includes at least one method that allows transition to the next phase.
//
// Use of strong typing like this helps guide the consumer of the api package
// to construct a valid build sequence.
//

type Phase0 interface {
	AddIntOption(option string, value int) Phase0
	AddBoolOption(option string, value bool) Phase0
	SetDriver(device string) Phase0
	Run(loop EventLoop) int
}

type EventLoop func(API)

type Notification struct {
	notification *C.Notification
}

func (self Notification) String() string {
	return fmt.Sprintf(
		"Notification[\n"+
			"notificationType=%s,\n"+
			"notificationCode=%s,\n"+
			"homeId=0x%08x,\n"+
			"nodeId=0x%02x,\n"+
			"valueType=%s,\n"+
			"valueId=0x%08x]\n",
		NT.ToEnum(int(self.notification.notificationType)),
		CODE.ToEnum(int(self.notification.notificationCode)),
		self.notification.nodeId.homeId,
		self.notification.nodeId.nodeId,
		VT.ToEnum(int(self.notification.valueId.valueType)),
		self.notification.valueId.valueId)
}

// allocate the control block used to track the state of the API
func BuildAPI(configPath string, userPath string, overrides string) Phase0 {
	var (
		cConfigPath *C.char = C.CString(configPath)
		cUserPath   *C.char = C.CString(userPath)
		cOverrides  *C.char = C.CString(overrides)
	)
	//defer C.free(unsafe.Pointer(cConfigPath))
	//defer C.free(unsafe.Pointer(cUserPath))
	//defer C.free(unsafe.Pointer(cOverrides))
	return api{
		C.startOptions(cConfigPath, cUserPath, cOverrides),
		make(chan Notification),
		defaultDriverName,
		make(chan bool, 0),
		C.Manager{nil}}
}

// configure the C++ Options object with an integer value
func (self api) AddIntOption(option string, value int) Phase0 {
	var cOption *C.char = C.CString(option)
	//defer C.free(unsafe.Pointer(cOption))

	C.addIntOption(self.options, cOption, C.int(value))
	return self
}

// configure the C++ Options object with a boolean value
func (self api) AddBoolOption(option string, value bool) Phase0 {
	var cOption *C.char = C.CString(option)

	//defer C.free(unsafe.Pointer(cOption))
	C.addBoolOption(self.options, cOption, C._Bool(value))
	return self
}

// add a driver.
func (self api) SetDriver(device string) Phase0 {
	if device != "" {
		self.device = device
	}
	return self
}

//
// Run the supplied event loop
//
// The intent of the complexity is to gracefully handle device insertion and removal events and to
// deal with unexpected (but observed) lockups during the driver removal processing.
//
// The function will only return if a signal is received. It may also call os.Exit(1) in case
// of unexpected lock ups during signal handling, device or driver removal processing.
//
func (self api) Run(loop EventLoop) int {

	// lock the options object, now we are done configuring it

	C.endOptions(self.options)

	// allocate various channels we need

	signals := make(chan os.Signal, 1) // used to receive OS signals
	startQuit := make(chan bool, 2)    // used to indicate we need to quit the event loop
	signalRaised := make(chan bool, 1) // used to indicate to outer loop that it should exit
	exit := make(chan int, 1)          // used to indicate we are ready to exit

	// indicate that we want to wait for these signals

	signal.Notify(signals, os.Interrupt, os.Kill)

	//
	// This goroutine does the following
	//    starts the manager
	//    starts a device monitoroing loop which
	//       waits for the device to be available
	// 	 starts a device removal goroutine which raises a startQuit signal when removal of the device is detected
	//   	 starts the driver
	//	 starts a go routine that that waits until a startQuit is signaled, then initiates the removal of the driver and quit of the event loop
	//	 runs the event loop
	//
	// It does not exit until either an OS Interrupt or Kill signal is received or driver removal or event loop blocks for some reason.
	//
	// If the device is removed, the monitoring go routine will send a signal via the startQuit channel. The intent is to allow the
	// event loop to exit and have the driver removed.
	//
	// The driver removal goroutine waits for the startQuit signal, then attempts to remove the driver. If this completes successfully
	// it propagates a quit signal to the event loop. It also sets up an abort timer which will exit the process if either
	// the driver removal or quit signal propagation blocks for some reason.
	//
	// If an OS signal is received, the main go routine will send signals to the startQuit and to the signalRaised channels.
	// It then waits for another signal, for the outer loop to exit or for a 5 second timeout. When one of these occurs, the
	// process will exit.
	//

	go func() {
		cSelf := unsafe.Pointer(&self) // a reference to self

		self.manager = C.startManager(cSelf) // start the manager
		defer C.stopManager(self.manager, cSelf)

		cDevice := C.CString(self.device) // allocate a C string for device
		defer C.free(unsafe.Pointer(cDevice))

		// a function which returns true if the device exists
		deviceExists := func() bool {
			if _, err := os.Stat(self.device); err == nil {
				return true
			} else {
				if os.IsNotExist(err) {
					return false
				} else {
					return true
				}
			}
		}

		// waits until the state matches the desired state.
		pollUntilDeviceExistsStateEquals := func(comparand bool) {
			for deviceExists() != comparand {
				time.Sleep(time.Second)
			}
		}

		// there is one iteration of this loop for each device insertion/removal cycle
		done := false
		for !done {
			select {
			case done = <-signalRaised: // we received a signal, allow us to quit
				break
			default:
				// one iteration of a device insert/removal cycle

				// wait until device present
				fmt.Printf("waiting until %s is available\n", self.device)
				pollUntilDeviceExistsStateEquals(true)

				go func() {

					// wait until device absent
					pollUntilDeviceExistsStateEquals(false)
					fmt.Printf("device %s removed\n", self.device)

					// start the removal of the driver
					startQuit <- true
				}()

				C.addDriver(self.manager, cDevice)

				go func() {
					// wait until something (OS signal handler or device existence monitor) decides we need to terminate
					<-startQuit

					// we start an abort timer, because if the driver blocks, we need to start the driver process
					abortTimer := time.AfterFunc(5*time.Second, func() {
						fmt.Printf("failed to remove driver - exiting driver process\n")
						os.Exit(1)
					})

					// try to remove the driver
					if C.removeDriver(self.manager, cDevice) {
						self.quit <- true
						abortTimer.Stop() // if we get to here in a timely fashion we can stop the abort timer
					} else {
						// this is unexpected, if we get to here, let the abort timer do its thing
						fmt.Printf("removeDriver call failed - waiting for abort\n")
					}
				}()

				loop(self) // run the event loop
			}
		}

		exit <- 1
	}()

	// Block until a signal is received.

	signal := <-signals
	fmt.Printf("received %v signal - commencing shutdown\n", signal)

	startQuit <- true    // try a graceful shutdown of the event loop
	signalRaised <- true // ensure the device existence loop will exit

	// but, just in case this doesn't happen, set up an abort timer.
	time.AfterFunc(time.Second*5, func() {
		fmt.Printf("timed out while waiting for event loop to quit - aborting now\n")
		exit <- 1
	})

	for {
		select {
		// the device existence loop has exited
		case rc := <-exit:
			return rc
		// the user is impatient - just die now
		case signal := <-signals:
			fmt.Printf("received 2nd %v signal - aborting now\n", signal)
			return 1
		}
	}
}

func (self api) Notifications() chan Notification {
	return self.notifications
}

func (self api) QuitSignal() chan bool {
	return self.quit
}

func (self api) FreeNotification(apiNotification Notification) {
	C.freeNotification(apiNotification.notification)
}

//export onNotificationWrapper
func onNotificationWrapper(notification *C.Notification, context unsafe.Pointer) {
	self := (*api)(context)
	self.notifications <- Notification{notification}
}
