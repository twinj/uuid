package uuid

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"crypto/rand"
)

var (
	generator *Generator
)

func init() {
	registerDefaultGenerator()
}

func NewGenerator (
	fRandom func([]byte) (int, error),
	fNext func() Timestamp,
	fId func() Node) (generator *Generator) {
		generator = new(Generator)
	if fRandom == nil {
		generator.Random = rand.Read
	} else {
		generator.Random = fRandom
	}
		generator.Next = fNext
		generator.Id = fId
	return
}

func registerDefaultGenerator() {
	generator = NewGenerator(
		rand.Read,
		(&spinner{
			Resolution: defaultSpinResolution,
			Timestamp:  Now(),
			Count:      0,
		}).next,
		findFirstHardwareAddress)
}

// Init will initialise the default generator
func Init() error {
	generator.Do(generator.init)
	return generator.Error()
}

func RegisterGenerator(pGenerator *Generator) error {
	generator = pGenerator
	return nil
}

// an iterated value to help ensure unique UUID generations values
// across the same domain, server restarts and clock issues
type Sequence uint16

// the last node id setup used by the generator
type Node []byte

// Store is used for storage of UUID generation history to ensure continuous
// running of the UUID generator between restarts and to monitor synchronicity
// while generating new V1 or V2 UUIDs
type Store struct {
	Timestamp
	Sequence
	Node
}

// String returns a string representation of the Store
func (o Store) String() string {
	return fmt.Sprintf("Timestamp[%s]-Sequence[%d]-Node[%x]", o.Timestamp, o.Sequence, o.Node)
}

// Saver is an interface to setup a non volatile store within your system
// if you wish to use V1 and V2 UUIDs based on your node id and a constant time
// it is highly recommended to implement this.
// A default implementation has been provided. FileSystemStorage, the default
// behaviour of the package is to generate random sequences where a Saver is not
// specified.
type Saver interface {
	// Read is run once, use this to setup your UUID state machine
	// Read should also return the UUID state from the non volatile store
	Read() (error, Store)

	// Save saves the state to the non volatile store and is called only if
	Save(Store)
}

// RegisterSaver register's a uuid.Saver implementation to the default package
// uuid.Generator. If you wish to save the generator state, this function must
// be run before any calls to V1 or V2 UUIDs. uuid.RegisterSaver cannot be run
// in conjunction with uuid.Init. You may implement the uuid.Saver interface
// or use the provided uuid.saver's from the uuId/savers package.
func RegisterSaver(pSaver Saver) {
	generator.Do(func() {
		defer generator.init()
		generator.Lock()
		defer generator.Unlock()
		generator.Saver = pSaver
	})
}

type Random func([]byte)(int, error)
type Next func() Timestamp
type Id func() Node
type HandleError func(error) Uuid

// Generator is used to create and monitor the running of V1 and V2, and V4
// UUIDs. It can be setup to take different implementations for Timestamp, Node
// and CPRNG retrieval. This is also where the Saver implementation can be
// given.
type Generator struct {

	// Access to the store needs to be maintained
	sync.Mutex

	// Once ensures that the generator is only setup and initialised once.
	// This will occur either when you explicitly call the
	// uuid.Generator.Init function or when a V1 or V2 id is generated.
	sync.Once

	err    error

	// Store contains the current values being used by the Generator.
	*Store

	// Intended to provide a non-volatile store to save the state of the
	// generator, the default is nil and to therefore generate a timestamp
	// clock sequence with random data. You can register your own save by
	// using the uuid.RegisterSaver function or by creating your own
	// uuid.Generator instance from which to generate your V1, V2 or V4
	// UUIDs.
	Saver

	// Id provides the Node to be used during the life of the generator. If
	// it cannot be determined nil should be returned, the package will
	// then provide a crypto-random node id. The default gets a MAC address
	// from any interface that is up using net.FlagUp.
	Id

	// HandleError provides the user the ability to manage any serious
	// error that may be caused by accessing the standard crypto/rand
	// library. Due to the rarity of this occurrence the error is swallowed
	// by NewV1, NewV2 or NewV4 methods which rely on random numbers, V4 in
	// particular.
	//
	// If you have correctly called uuid.Init or called it on your custom
	// uuid.Generator then the error will return through that function..
	//
	// The UUID functions mentioned will return nil by default. This
	// function can be used to manage the issue through delegation
	// where you can recover at some level in your program. Issues with the
	// RNG in an OS. If this function is not provided the UUID methods will
	// panic on any error.
	HandleError

	// Random provides a CPRNG which reads into the given []byte, the package
	// uses crypto/rand.Read by default. You can supply your own CPRNG.
	Random

	// Next provides the next Timestamp value to be used by the next UUID.
	// The default uses the uuid.spinner which spins at a resolution of
	// 100ns ticks and provides a spin resolution redundancy with 1024
	// cycles. This ensures that the system is not too quick when
	// generating V1 or V2 UUIDs.
	Next
}

// Error will return any error from the uuid.Generator if a UUID returns as Nil
// or nil
func (o *Generator) Error() (err error) {
	err = o.err
	o.err = nil
	return
}

func (o *Generator) read() {

	// From a system-wide shared stable store (e.g., a file), read the
	// UUID generator state: the values of the timestamp, clock sequence,
	// and node ID used to generate the last UUID.
	o.Do(o.init)

	// Save the state (current timestamp, clock sequence, and node ID)
	// back to the stable store
	if o.Saver != nil {
		defer o.save()
	}

	// Obtain a lock
	o.Lock()
	defer o.Unlock()

	// Get the current time as a 60-bit count of 100-nanosecond intervals
	// since 00:00:00.00, 15 October 1582.
	now := o.Next()

	// If the last timestamp is later than
	// the current timestamp, increment the clock sequence value.
	if now < o.Timestamp {
		o.Sequence++
	}

	// Update the timestamp
	o.Timestamp = now
}

func (o *Generator) init() {
	// From a system-wide shared stable store (e.g., a file), read the
	// UUID generator state: the values of the timestamp, clock sequence,
	// and node ID used to generate the last UUID.
	var (
		storage Store
		err error
	)

	o.Lock()
	defer o.Unlock()

	if o.Saver != nil {
		err, storage = o.Read()
		if err != nil {
			o.Saver = nil
		}
	}

	// Get the current time as a 60-bit count of 100-nanosecond intervals
	// since 00:00:00.00, 15 October 1582.
	now := o.Next()

	//  Get the current node id
	node := o.Id()

	if node == nil {
		log.Println("uuid.Generator.init: address error: will generate random node id instead")

		node = make([]byte, 6)
		n, err := o.Random(node)
		if err != nil {
			log.Printf("uuid.Generator.init: could not read random bytes into node - read [%d] %s", n, err)
			o.err = err
			return
		}
		// Mark as randomly generated
		node[0] |= 0x01
	}

	// If the state was unavailable (e.g., non-existent or corrupted), or
	// the saved node ID is different than the current node ID, generate
	// a random clock sequence value.
	if o.Saver == nil || !bytes.Equal(storage.Node, node) {

		// 4.1.5.  Clock Sequence https://www.ietf.org/rfc/rfc4122.txt
		//
		// For UUID version 1, the clock sequence is used to help avoid
		// duplicates that could arise when the clock is set backwards in time
		// or if the node ID changes.
		//
		// If the clock is set backwards, or might have been set backwards
		// (e.g., while the system was powered off), and the UUID generator can
		// not be sure that no UUIDs were generated with timestamps larger than
		// the value to which the clock was set, then the clock sequence has to
		// be changed.  If the previous value of the clock sequence is known, it
		// can just be incremented; otherwise it should be set to a random or
		// high-quality pseudo-random value.

		// The clock sequence MUST be originally (i.e., once in the lifetime of
		// a system) initialized to a random number to minimize the correlation
		// across systems.  This provides maximum protection against node
		// identifiers that may move or switch from system to system rapidly.
		// The initial value MUST NOT be correlated to the node identifier.
		b := make([]byte, 2)
		n, err := o.Random(b)
		if err == nil {
			storage.Sequence = Sequence(binary.BigEndian.Uint16(b))
			log.Printf("uuid.Generator.init initialised random sequence: [%d]", storage.Sequence)

		} else {
			log.Printf("uuid.Generator.init: could not read random bytes into sequence - read [%d] %s", n, err)
			o.err = err
			return
		}
	} else if now < storage.Timestamp {
		// If the state was available, but the saved timestamp is later than
		// the current timestamp, increment the clock sequence value.
		storage.Sequence++
	}

	storage.Timestamp = now
	storage.Node = node

	o.Store = &storage
}

func (o *Generator) save() {
	func(pState *Generator) {
		if pState.Saver != nil {
			pState.Lock()
			defer pState.Unlock()
			pState.Save(*pState.Store)
		}
	}(o)
}

// NewV1 generates a new RFC4122 version 1 UUID based on a 60 bit timestamp and
// node id
func (o *Generator) NewV1() Uuid {
	o.read()
	id := array{}

	makeUuid(&id,
		uint32(o.Timestamp),
		uint16(o.Timestamp >> 32),
		uint16(o.Timestamp >> 48),
		uint16(o.Sequence),
		o.Node)

	id.setRFC4122Version(1)
	return id[:]
}

// NewV2 generates a new DCE version 2 UUID based on a 60 bit timestamp, node id
// and POSIX UID or GID
func (o *Generator) NewV2(pDomain Domain) Uuid {
	o.read()

	id := array{}

	var domain uint32

	switch pDomain {
	case DomainUser:
		domain = uint32(os.Getuid())
	case DomainGroup:
		domain = uint32(os.Getgid())
	}

	makeUuid(&id,
		domain,
		uint16(o.Timestamp >> 32),
		uint16(o.Timestamp >> 48),
		uint16(o.Sequence),
		o.Node)

	id[9] = byte(pDomain)
	id.setRFC4122Version(2)
	return id[:]
}

func makeUuid(pId *array, pLow uint32, pMid, pHiAndV, seq uint16, pNode Node) {

	pId[0] = byte(pLow >> 24)
	pId[1] = byte(pLow >> 16)
	pId[2] = byte(pLow >> 8)
	pId[3] = byte(pLow)

	pId[4] = byte(pMid >> 8)
	pId[5] = byte(pMid)

	pId[6] = byte(pHiAndV >> 8)
	pId[7] = byte(pHiAndV)

	pId[8] = byte(seq >> 8)
	pId[9] = byte(seq)

	copy(pId[10:], pNode)
}

func findFirstHardwareAddress() (node Node) {
	interfaces, err := net.Interfaces()
	if err == nil {
		for _, i := range interfaces {
			if i.Flags & net.FlagUp != 0 && bytes.Compare(i.HardwareAddr, nil) != 0 {
				// Don't use random as we have a real address
				node = Node(i.HardwareAddr)
				log.Println("uuid.getHardwareAddress:", node)
				break
			}
		}
	}
	return
}
