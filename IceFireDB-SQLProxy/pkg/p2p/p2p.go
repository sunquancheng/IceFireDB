package p2p

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	discoveryRouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/mr-tron/base58/base58"
	"github.com/multiformats/go-multihash"
	"github.com/sirupsen/logrus"
)

// P2P A structure that represents a P2P Host

type P2P struct {
	// Represents the host context layer
	Ctx context.Context

	// Represents the libp2p host
	Host host.Host

	// Represents the DHT routing table
	KadDHT *dht.IpfsDHT

	// Represents the peer discovery service
	Discovery *discoveryRouting.RoutingDiscovery

	// Represents the PubSub Handler
	PubSub *pubsub.PubSub

	service string
}

/*
A constructor function that generates and returns a P2P object.

Constructs a libp2p host with TLS encrypted secure transportation that works over a TCP
transport connection using a Yamux Stream Multiplexer and uses UPnP for the NAT traversal.

A Kademlia DHT is then bootstrapped on this host using the default peers offered by libp2p
and a Peer Discovery service is created from this Kademlia DHT. The PubSub handler is then
created on the host using the peer discovery service created prior.
*/

func NewP2P(serviceName string, nodeHostIP string, nodeHostPort int) *P2P {
	// Setup a background context
	ctx := context.Background()

	// Setup a P2P Host Node
	nodehost, kaddht := setupHost(ctx, nodeHostIP, nodeHostPort)
	// Debug log
	logrus.Infoln("Setup the p2p host,listen on", nodehost.Addrs())
	log.Println("MY P2P Node ID", nodehost.ID())

	// Bootstrap the Kad DHT
	bootstrapDHT(ctx, nodehost, kaddht)

	// Debug log
	logrus.Debugln("Bootstrapped the Kademlia DHT and Connected to Bootstrap Peers")

	// Create a peer discovery service using the Kad DHT
	routingdiscovery := discoveryRouting.NewRoutingDiscovery(kaddht)
	// Debug log
	logrus.Debugln("Created the Peer Discovery Service.")

	// Create a PubSub handler with the routing discovery PubSu
	pubsubhandler := setupPubSub(ctx, nodehost, routingdiscovery)
	// Debug log
	logrus.Debugln("Created the PubSub Handler.")

	// Return the P2P object
	return &P2P{
		Ctx:       ctx,
		Host:      nodehost,
		KadDHT:    kaddht,
		Discovery: routingdiscovery,
		PubSub:    pubsubhandler,
		service:   serviceName,
	}
}

// A method of P2P to connect to service peers.
// This method uses the Advertise() functionality of the Peer Discovery Service
// to advertise the service and then disovers all peers advertising the same.
// The peer discovery is handled by a go-routine that will read from a channel
// of peer address information until the peer channel closes
func (p2p *P2P) AdvertiseConnect() {
	// Advertise the availabilty of the service on this node
	ttl, err := p2p.Discovery.Advertise(p2p.Ctx, p2p.service)
	// Debug log
	logrus.Debugln("Advertised the p2p Service.")
	// Sleep to give time for the advertisment to propogate
	time.Sleep(time.Second * 5)
	// Debug log
	logrus.Debugf("Service Time-to-Live is %s", ttl)

	// Find all peers advertising the same service
	peerchan, err := p2p.Discovery.FindPeers(p2p.Ctx, p2p.service)
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("P2P Peer Discovery Failed!")
	}
	// Trace log
	logrus.Traceln("Discovered p2p Service Peers.")

	// Connect to peers as they are discovered
	go handlePeerDiscovery(p2p.Host, peerchan)
	// Trace log
	logrus.Traceln("Started Peer Connection Handler.")
}

// A method of P2P to connect to service peers.
// This method uses the Provide() functionality of the Kademlia DHT directly to announce
// the ability to provide the service and then disovers all peers that provide the same.
// The peer discovery is handled by a go-routine that will read from a channel
// of peer address information until the peer channel closes
func (p2p *P2P) AnnounceConnect() {
	// Generate the Service CID
	cidvalue := generateCID(p2p.service)
	// Trace log
	logrus.Debug("cidvalue ", cidvalue.String())
	logrus.Traceln("Generated the Service CID.")

	// Announce that this host can provide the service CID
	err := p2p.KadDHT.Provide(p2p.Ctx, cidvalue, true)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Announce Service CID!")
	}
	// Debug log
	logrus.Debugln("Announced the p2p Service.")
	// Sleep to give time for the advertisment to propogate
	time.Sleep(time.Second * 5)

	// Find the other providers for the service CID
	peerchan := p2p.KadDHT.FindProvidersAsync(p2p.Ctx, cidvalue, 0)
	// Trace log
	logrus.Traceln("Discovered p2p Service Peers.")

	// Connect to peers as they are discovered
	go handlePeerDiscovery(p2p.Host, peerchan)
	// Debug log
	logrus.Debugln("Started Peer Connection Handler.")
}

// A function that generates the p2p configuration options and creates a
// libp2p host object for the given context. The created host is returned
func setupHost(ctx context.Context, nodeHostIP string, nodeHostPort int) (host.Host, *dht.IpfsDHT) {
	// Set up the host identity options
	prvkey, pubkey, err := crypto.GenerateKeyPair(
		crypto.Ed25519, // Select your key type. Ed25519 are nice short
		-1,             // Select key length when possible (i.e. RSA).
	)

	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Generate P2P Identity Configuration!")
	}

	_ = pubkey

	// identity := libp2p.Identity(prvkey)

	// // Trace log
	// logrus.Traceln("Generated P2P Identity Configuration.")

	// // Set up TLS secured TCP transport and options
	// tlstransport, err := libp2ptls.New(libp2ptls.ID, prvkey, nil)
	// security := libp2p.Security(libp2ptls.ID, tlstransport)
	// transport := libp2p.Transport(tcp.NewTCPTransport)
	// Handle any potential error
	// if err != nil {
	// 	logrus.WithFields(logrus.Fields{
	// 		"error": err.Error(),
	// 	}).Fatalln("Failed to Generate P2P Security and Transport Configurations!")
	// }

	// Trace log
	// logrus.Traceln("Generated P2P Security and Transport Configurations.")

	// multiaddrStr := "/ip4/%s/tcp/%d"

	// //default "/ip4/0.0.0.0/tcp/0"
	// multiaddrStr = fmt.Sprintf(multiaddrStr, nodeHostIP, nodeHostPort)

	// // Set up host listener address options
	// muladdr, err := multiaddr.NewMultiaddr(multiaddrStr)

	// logrus.Infoln("Setup Multiaddr", multiaddrStr)

	// listen := libp2p.ListenAddrs(muladdr)
	// // Handle any potential error
	// if err != nil {
	// 	logrus.WithFields(logrus.Fields{
	// 		"error": err.Error(),
	// 	}).Fatalln("Failed to Generate P2P Address Listener Configuration!")
	// }

	// // Trace log
	// logrus.Traceln("Generated P2P Address Listener Configuration.")
	// // Set up the stream multiplexer and connection manager options
	// muxer := libp2p.Muxer("/yamux/1.0.0", yamux.DefaultTransport)
	// basicConnMgr, err := connmgr.NewConnManager(100, 400, connmgr.WithGracePeriod(time.Minute))
	// if err != nil {
	// 	logrus.WithFields(logrus.Fields{
	// 		"error": err.Error(),
	// 	}).Fatalln("Failed to NewConnManager!")
	// }
	// conn := libp2p.ConnectionManager(basicConnMgr)
	// // Trace log
	// logrus.Traceln("Generated P2P Stream Multiplexer, Connection Manager Configurations.")

	// Setup NAT traversal and relay options
	// nat := libp2p.NATPortMap()
	// relay := libp2p.EnableAutoRelay()

	// // Trace log
	// logrus.Traceln("Generated P2P NAT Traversal and Relay Configurations.")

	// // Declare a KadDHT
	// var kaddht *dht.IpfsDHT
	// // Setup a routing configuration with the KadDHT
	// routingOpt := libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
	// 	kaddht = setupKadDHT(ctx, h)
	// 	return kaddht, err
	// })

	// Trace log
	//logrus.Traceln("Generated P2P Routing Configurations.")

	//opts := libp2p.ChainOptions(identity, listen, security, transport, muxer, conn, nat, routingOpt, relay)
	//opts := libp2p.ChainOptions(identity, listen, security, transport, muxer, conn, nat, routingOpt, relay)
	// Construct a new libP2P host with the created options
	// libhost, err := libp2p.New(opts)
	// // Handle any potential error
	// if err != nil {
	// 	logrus.WithFields(logrus.Fields{
	// 		"error": err.Error(),
	// 	}).Fatalln("Failed to Create the P2P Host!")
	// }

	//test new libp2p
	// _ = libhost

	var idht *dht.IpfsDHT

	connmgr, err := connmgr.NewConnManager(
		100, // Lowwater
		400, // HighWater,
		connmgr.WithGracePeriod(time.Minute),
	)

	if err != nil {
		panic(err)
	}

	tcpListenAddress := fmt.Sprintf("/ip4/%s/tcp/%d", nodeHostIP, nodeHostPort)
	quicListenAddress := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", nodeHostIP, nodeHostPort)

	log.Println(tcpListenAddress, quicListenAddress)

	h2, err := libp2p.New(
		// Use the keypair we generated
		libp2p.Identity(prvkey),
		// Multiple listen addresses
		libp2p.ListenAddrStrings(
			tcpListenAddress,  // regular tcp connections
			quicListenAddress, // a UDP endpoint for the QUIC transport
		),
		// support TLS connections
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		// support noise connections
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer("/yamux/1.0.0", yamux.DefaultTransport),
		// support any other default transports (TCP)
		libp2p.DefaultTransports,
		// Let's prevent our peer from having too many
		// connections by attaching a connection manager.
		libp2p.ConnectionManager(connmgr),
		// Attempt to open ports using uPNP for NATed hosts.
		libp2p.NATPortMap(),
		// Let this host use the DHT to find other hosts
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			idht, err = setupKadDHT(ctx, h)
			return idht, err
		}),
		// If you want to help other peers to figure out if they are behind
		// NATs, you can launch the server-side of AutoNAT too (AutoRelay
		// already runs the client)
		//
		// This service is highly rate-limited and should not cause any
		// performance issues.
		libp2p.EnableNATService(),
		libp2p.EnableAutoNATv2(),
		libp2p.EnableRelay(),
		// libp2p.EnableAutoRelay(),
	)
	if err != nil {
		panic(err)
	}
	//defer h2.Close()

	// Return the created host and the kademlia DHT
	return h2, idht
}

// A function that generates a Kademlia DHT object and returns it
func setupKadDHT(ctx context.Context, nodehost host.Host) (*dht.IpfsDHT, error) {
	// Create DHT server mode option
	dhtmode := dht.Mode(dht.ModeServer)
	// Rertieve the list of boostrap peer addresses
	bootstrappeers := dht.GetDefaultBootstrapPeerAddrInfos()
	// Create the DHT bootstrap peers option
	dhtpeers := dht.BootstrapPeers(bootstrappeers...)

	// Trace log
	logrus.Traceln("Generated DHT Configuration.")

	// Start a Kademlia DHT on the host in server mode
	kaddht, err := dht.New(ctx, nodehost, dhtmode, dhtpeers)
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Create the Kademlia DHT!")
	}

	// Return the KadDHT
	return kaddht, err
}

// A function that generates a PubSub Handler object and returns it
// Requires a node host and a routing discovery service.
func setupPubSub(ctx context.Context, nodehost host.Host, routingdiscovery *discoveryRouting.RoutingDiscovery) *pubsub.PubSub {
	// Create a new PubSub service which uses a GossipSub router
	pubsubhandler, err := pubsub.NewGossipSub(ctx, nodehost, pubsub.WithDiscovery(routingdiscovery))
	// Handle any potential error
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
			"type":  "GossipSub",
		}).Fatalln("PubSub Handler Creation Failed!")
	}

	// Return the PubSub handler
	return pubsubhandler
}

// A function that bootstraps a given Kademlia DHT to satisfy the IPFS router
// interface and connects to all the bootstrap peers provided by libp2p
func bootstrapDHT(ctx context.Context, nodehost host.Host, kaddht *dht.IpfsDHT) {
	// Bootstrap the DHT to satisfy the IPFS Router interface
	if err := kaddht.Bootstrap(ctx); err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Bootstrap the Kademlia!")
	}

	// Trace log
	logrus.Traceln("Set the Kademlia DHT into Bootstrap Mode.")

	// Declare a WaitGroup
	var wg sync.WaitGroup
	// Declare counters for the number of bootstrap peers
	var connectedbootpeers int32
	var totalbootpeers int32

	// Iterate over the default bootstrap peers provided by libp2p
	for _, peeraddr := range dht.DefaultBootstrapPeers {
		// Retrieve the peer address information
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peeraddr)

		// Incremenent waitgroup counter
		wg.Add(1)
		totalbootpeers++
		// Start a goroutine to connect to each bootstrap peer
		go func() {
			// Defer the waitgroup decrement
			defer wg.Done()
			// Attempt to connect to the bootstrap peer
			if err := nodehost.Connect(ctx, *peerinfo); err == nil {
				// Increment the connected bootstrap peer count
				atomic.AddInt32(&connectedbootpeers, 1)
				// log.Println("Connected bootstrap peer success.", peerinfo.ID, peerinfo)
			}
		}()
	}

	// Wait for the waitgroup to complete
	wg.Wait()

	// Log the number of bootstrap peers connected
	logrus.Debugf("Connected to %d out of %d Bootstrap Peers.", connectedbootpeers, totalbootpeers)
}

// A function that connects the given host to all peers received from a
// channel of peer address information. Meant to be started as a go routine.
func handlePeerDiscovery(nodehost host.Host, peerchan <-chan peer.AddrInfo) {
	// Iterate over the peer channel
	for peer := range peerchan {
		// Ignore if the discovered peer is the host itself
		if peer.ID == nodehost.ID() {
			continue
		}

		// Connect to the peer
		err := nodehost.Connect(context.Background(), peer)

		if err != nil {
			logrus.Debugln("p2p peer connection failed: ", err)
		}

		logrus.Debugln("p2p peer connection success: ", peer.ID)
		log.Println("p2p peer connection success: ", peer.ID)
	}
}

// A function that generates a CID object for a given string and returns it.
// Uses SHA256 to hash the string and generate a multihash from it.
// The mulithash is then base58 encoded and then used to create the CID
func generateCID(namestring string) cid.Cid {
	// Hash the service content ID with SHA256
	hash := sha256.Sum256([]byte(namestring))
	// Append the hash with the hashing codec ID for SHA2-256 (0x12),
	// the digest size (0x20) and the hash of the service content ID
	finalhash := append([]byte{0x12, 0x20}, hash[:]...)
	// Encode the fullhash to Base58
	b58string := base58.Encode(finalhash)

	// Generate a Multihash from the base58 string
	mulhash, err := multihash.FromB58String(string(b58string))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatalln("Failed to Generate Service CID!")
	}

	// Generate a CID from the Multihash
	cidvalue := cid.NewCidV1(12, mulhash)
	// Return the CID
	return cidvalue
}
