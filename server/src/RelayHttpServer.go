package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"io/ioutil"
	"librelay"
	"librelay/txstore"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const VERSION = "0.4.0"

var KeystoreDir = filepath.Join(os.Getenv("PWD"), "build/server/keystore")
var delayBetweenRegistrations = 24 * int64(time.Hour/time.Second) // time.Duration is in nanosec - converting to sec like unix
var shortSleep bool                                               // Whether we wait after calls to blockchain or return (almost) immediately. Usually when testing...

var ready = false
var removed = false

var relay librelay.IRelay
var server *http.Server
var stopKeepAlive chan bool
var stopRefreshBlockchainView chan bool
var stopUpdatingPendingTxs chan bool
var stopListeningToRelayRemoved chan bool

var timeUnit time.Duration

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("RelayHttpServer starting. version:", VERSION)

	configRelay(parseCommandLine())

	server = &http.Server{Addr: ":" + relay.GetPort(), Handler: nil}

	http.HandleFunc("/relay", assureRelayReady(relayHandler))
	http.HandleFunc("/getaddr", getEthAddrHandler)

	timeUnit = time.Minute
	if shortSleep {
		timeUnit = 100 * time.Millisecond
	}
	stopKeepAlive = schedule(keepAlive, 1*timeUnit, 0)
	stopRefreshBlockchainView = schedule(refreshBlockchainView, 1*timeUnit, 0)
	stopUpdatingPendingTxs = schedule(updatePendingTxs, 1*timeUnit, 0)
	stopListeningToRelayRemoved = schedule(stopServingOnRelayRemoved, 1*timeUnit, 0)

	log.Println("RelayHttpServer started.Listening on port: ", relay.GetPort())
	err := server.ListenAndServe()
	if err != nil {
		log.Fatalln(err)
	}

}

// http.HandlerFunc wrapper to assure we have enough balance to operate, and server already has stake and registered
func assureRelayReady(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		w.Header()["Access-Control-Allow-Origin"] = []string{"*"}
		w.Header()["Access-Control-Allow-Headers"] = []string{"Content-Type, Authorization, Content-Length, X-Requested-With"}
		w.Header()["Access-Control-Allow-Methods"] = []string{"GET, POST, OPTIONS"}

		if !shouldHandleRelayRequests() {
			err := fmt.Errorf("Relay not staked and registered yet")
			log.Println(err)
			w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
			return
		}

		// wait for funding
		balance, err := relay.Balance()
		if err != nil {
			log.Println(err)
			w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
			return
		}
		if balance.Uint64() == 0 {
			err = fmt.Errorf("Waiting for funding...")
			log.Println(err)
			w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
			return
		}
		log.Println("Relay balance:", balance.Uint64())

		gasPrice := relay.GasPrice()
		if gasPrice.Uint64() == 0 {
			err = fmt.Errorf("Waiting for gasPrice...")
			log.Println(err)
			w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
			return
		}
		log.Println("Relay received gasPrice::", gasPrice.Uint64())
		fn(w, r)
	}

}

func getEthAddrHandler(w http.ResponseWriter, _ *http.Request) {

	w.Header()["Access-Control-Allow-Origin"] = []string{"*"}
	w.Header()["Access-Control-Allow-Headers"] = []string{"Content-Type, Authorization, Content-Length, X-Requested-With"}
	w.Header()["Access-Control-Allow-Methods"] = []string{"GET, OPTIONS"}

	getEthAddrResponse := &librelay.GetEthAddrResponse{
		RelayServerAddress: relay.Address(),
		MinGasPrice:        relay.GasPrice(),
		Ready:              shouldHandleRelayRequests(),
		Version:            VERSION,
	}
	resp, err := json.Marshal(getEthAddrResponse)
	if err != nil {
		log.Println(err)
		w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
		return
	}
	log.Printf("address %s sent\n", relay.Address().Hex())

	w.Write(resp)
}

func relayHandler(w http.ResponseWriter, r *http.Request) {

	log.Println("Relay Handler Start")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK)
		return
	}
	body, err := ioutil.ReadAll(r.Body)

	if err != nil {
		log.Println("Could not read request body", body, err)
		w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
		return
	}
	var request = &librelay.RelayTransactionRequest{}
	err = json.Unmarshal(body, request)
	if err != nil {
		log.Println("Invalid json", body, err)
		w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
		return
	}
	signedTx, err := relay.CreateRelayTransaction(*request)
	if err != nil {
		log.Println("Failed to relay")
		w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))

		return
	}
	resp, err := signedTx.MarshalJSON()
	if err != nil {
		log.Println(err)
		w.Write([]byte("{\"error\":\"" + err.Error() + "\"}"))
		return
	}
	w.Write(resp)
}

func parseCommandLine() (relayParams librelay.RelayParams) {
	ownerAddress := flag.String("OwnerAddress", common.HexToAddress("0").Hex(), "Relay's owner address")
	fee := flag.Int64("Fee", 11, "Relay's per transaction fee")
	urlStr := flag.String("Url", "http://localhost:8090", "Relay server's url ")
	port := flag.String("Port", "", "Relay server's port")
	relayHubAddress := flag.String("RelayHubAddress", "0x254dffcd3277c0b1660f6d42efbb754edababc2b", "RelayHub address")
	stakeAmount := flag.Int64("StakeAmount", 10000000000000000, "Relay's stake (in wei)")
	gasLimit := flag.Uint64("GasLimit", 100000, "Relay's gas limit per transaction")
	defaultGasPrice := flag.Int64("DefaultGasPrice", int64(params.GWei), "Relay's default gasPrice per (non-relayed) transaction in wei")
	gasPricePercent := flag.Int64("GasPricePercent", 70, "Relay's gas price increase as percentage from current average. GasPrice = (100+GasPricePercent)/100 * eth_gasPrice() ")
	unstakeDelay := flag.Int64("UnstakeDelay", 1200, "Relay's time delay before being able to unsatke from relayhub (in days)")
	registrationBlockRate := flag.Uint64("RegistrationBlockRate", 5800, "Relay registeration rate (in blocks)")
	ethereumNodeUrl := flag.String("EthereumNodeUrl", "http://localhost:8545", "The relay's ethereum node")
	workdir := flag.String("Workdir", filepath.Join(os.Getenv("PWD"), "build/server"), "The relay server's workdir")
	flag.BoolVar(&shortSleep, "ShortSleep", false, "Whether we wait after calls to blockchain or return (almost) immediately")

	flag.Parse()

	relayParams.OwnerAddress = common.HexToAddress(*ownerAddress)
	relayParams.Fee = big.NewInt(*fee)
	relayParams.Url = *urlStr
	u, err := url.Parse(*urlStr)
	if err != nil {
		log.Fatalln("Could not parse url")
	}
	if *port == "" && u.Port() != "" {
		log.Println("Using default published port given in url:", *port)
		*port = u.Port()
	}

	relayParams.Port = *port
	relayParams.RelayHubAddress = common.HexToAddress(*relayHubAddress)
	relayParams.StakeAmount = big.NewInt(*stakeAmount)
	relayParams.GasLimit = *gasLimit
	relayParams.DefaultGasPrice = *defaultGasPrice
	relayParams.GasPricePercent = big.NewInt(*gasPricePercent)
	relayParams.UnstakeDelay = big.NewInt(*unstakeDelay)
	relayParams.RegistrationBlockRate = *registrationBlockRate
	relayParams.EthereumNodeURL = *ethereumNodeUrl
	relayParams.DBFile = filepath.Join(*workdir, "db")

	KeystoreDir = filepath.Join(*workdir, "keystore")

	log.Println("Using RelayHub address: " + relayParams.RelayHubAddress.String())
	log.Println("Using workdir: " + *workdir)
	log.Println("shortsleep? ", shortSleep)

	return relayParams

}

func configRelay(relayParams librelay.RelayParams) {
	log.Println("Constructing relay server in url ", relayParams.Url)
	privateKey := loadPrivateKey(KeystoreDir)
	log.Println("relay server address: ", crypto.PubkeyToAddress(privateKey.PublicKey).Hex())
	client, err := librelay.NewEthClient(relayParams.EthereumNodeURL, relayParams.DefaultGasPrice)
	if err != nil {
		log.Println("Could not connect to ethereum node", err)
		return
	}
	txStore, err := txstore.NewLevelDbTxStore(relayParams.DBFile, nil)
	if err != nil {
		log.Println("Could not create local transactions database", err)
		return
	}
	relay, err = librelay.NewRelayServer(
		relayParams.OwnerAddress, relayParams.Fee, relayParams.Url, relayParams.Port,
		relayParams.RelayHubAddress, relayParams.StakeAmount,
		relayParams.GasLimit, relayParams.DefaultGasPrice, relayParams.GasPricePercent,
		privateKey, relayParams.UnstakeDelay, relayParams.RegistrationBlockRate, relayParams.EthereumNodeURL,
		client, txStore, nil)
	if err != nil {
		log.Println("Could not create Relay Server", err)
		return
	}
}

// Wait for server to be staked & funded by owner, then try and register on RelayHub
func refreshBlockchainView() {
	if removed {
		log.Println("Relay removed. No need to wait for owner actions")
		return
	}
	waitForOwnerActions()
	log.Println("Waiting for registration...")
	when, err := relay.RegistrationDate()
	log.Println("when registered:", when, "unix:", time.Unix(when, 0))
	for ; err != nil || when == 0; when, err = relay.RegistrationDate() {
		if err != nil {
			log.Println(err)
		}
		ready = false
		sleep(15*time.Second, shortSleep)
	}

	log.Println("Trying to get gasPrice from node...")
	for err := relay.RefreshGasPrice(); err != nil; err = relay.RefreshGasPrice() {
		if err != nil {
			log.Println(err)
		}
		ready = false
		log.Println("Trying to get gasPrice from node again...")
		sleep(10*time.Second, shortSleep)

	}
	gasPrice := relay.GasPrice()
	log.Println("GasPrice:", gasPrice.Uint64())

	ready = true
}

func updatePendingTxs() {
	if removed {
		log.Println("Relay removed. No need to wait for owner actions")
		return
	}
	waitForOwnerActions()

	log.Println("Updating unconfirmed txs...")
	_, err := relay.UpdateUnconfirmedTransactions()
	if err != nil {
		log.Println("Error updating unconfirmed txs", err)
	}
}

func waitForOwnerActions() {
	if removed {
		log.Println("Relay removed. No need to wait for owner actions")
		return
	}
	staked, err := relay.IsStaked()
	for ; err != nil || !staked; staked, err = relay.IsStaked() {
		if err != nil {
			log.Println(err)
		}
		ready = false
		log.Println("Waiting for stake...")
		sleep(5*time.Second, shortSleep)
	}

	// wait for funding
	balance, err := relay.Balance()
	if err != nil {
		log.Println(err)
		return
	}
	for ; err != nil || balance.Uint64() <= 0.1*params.Ether; balance, err = relay.Balance() {
		ready = false
		log.Println("Server's balance too low. Waiting for funding...")
		sleep(10*time.Second, shortSleep)
	}
	log.Println("Relay funded. Balance:", balance)
}

func keepAlive() {
	if removed {
		log.Println("Relay removed. No need to reregister")
		return
	}
	waitForOwnerActions()
	when, err := relay.RegistrationDate()
	log.Println("when registered:", when, "unix:", time.Unix(when, 0))
	if err != nil {
		log.Println(err)
	} else if time.Now().Unix()-when < delayBetweenRegistrations {
		log.Println("Relay registered lately. No need to reregister")
		return
	}
	log.Println("Registering relay...")
	for {
		err := relay.RegisterRelay()
		if err == nil {
			break
		}
		log.Println(err)
		log.Println("Trying to register again...")
		sleep(1*time.Minute, shortSleep)
	}
	log.Println("Done registering")
}

func stopServingOnRelayRemoved() {
	var err error
	removed, err = relay.IsRemoved()
	if err != nil {
		log.Println(err)
		return
	}
	if removed {
		log.Println("Relay removed. Listening to Unstaked event")
		schedule(shutdownOnRelayUnstaked, 1*timeUnit, 0)
		stopListeningToRelayRemoved <- true
	}

}

func shutdownOnRelayUnstaked() {
	var err error
	removed, err = relay.IsUnstaked()
	if err != nil {
		log.Println(err)
		return
	}
	if removed {
		log.Println("Relay removed. Listening to Unstaked event")
		log.Println("Relay unstaked. Sending balance back to owner")
		sleep(2*time.Minute, shortSleep)
		for {
			err = relay.SendBalanceToOwner()
			if err == nil {
				break
			}
			sleep(5*time.Second, shortSleep)
		}
		server.Close()
	}

}

func shouldHandleRelayRequests() bool {
	return ready && !removed
}
