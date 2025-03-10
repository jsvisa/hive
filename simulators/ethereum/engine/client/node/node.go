package node

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"

	beacon "github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth"
	ethcatalyst "github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	ethprotocol "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/hive/hivesim"
	"github.com/ethereum/hive/simulators/ethereum/engine/client"
	client_types "github.com/ethereum/hive/simulators/ethereum/engine/client/types"
	"github.com/ethereum/hive/simulators/ethereum/engine/helper"
)

type GethNodeTestConfiguration struct {
	// Node name
	Name string

	// To allow testing terminal block gossiping on the other clients, restrict this client
	// to only connect to the boot node
	MaxPeers *big.Int

	// Max number of gossiped blocks to receive
	ExpectedGossipNewBlocksCount *big.Int

	// Chain to import
	ChainFile string
}
type GethNodeEngineStarter struct {
	// Client parameters used to launch the default client
	ChainFile               string
	TerminalTotalDifficulty *big.Int

	// Test specific configuration
	Config GethNodeTestConfiguration
}

var (
	DefaultMaxPeers = big.NewInt(25)
)

func (s GethNodeEngineStarter) StartClient(T *hivesim.T, testContext context.Context, genesis *core.Genesis, ClientParams hivesim.Params, ClientFiles hivesim.Params, bootClients ...client.EngineClient) (client.EngineClient, error) {
	return s.StartGethNode(T, testContext, genesis, ClientParams, ClientFiles, bootClients...)
}

func (s GethNodeEngineStarter) StartGethNode(T *hivesim.T, testContext context.Context, genesis *core.Genesis, ClientParams hivesim.Params, ClientFiles hivesim.Params, bootClients ...client.EngineClient) (*GethNode, error) {
	var (
		ttd = s.TerminalTotalDifficulty
		err error
	)

	if ttd == nil {
		if ttdStr, ok := ClientParams["HIVE_TERMINAL_TOTAL_DIFFICULTY"]; ok {
			// Retrieve TTD from parameters
			ttd, ok = new(big.Int).SetString(ttdStr, 10)
			if !ok {
				return nil, fmt.Errorf("Unable to parse TTD from parameters")
			}
		}
	} else {
		ttd = big.NewInt(helper.CalculateRealTTD(genesis, ttd.Int64()))
		ClientParams = ClientParams.Set("HIVE_TERMINAL_TOTAL_DIFFICULTY", fmt.Sprintf("%d", ttd))
	}

	// Not sure if this hack works
	genesisCopy := *genesis
	configCopy := *genesisCopy.Config
	configCopy.TerminalTotalDifficulty = ttd
	genesisCopy.Config = &configCopy

	var enodes []string
	if bootClients != nil && len(bootClients) > 0 {
		enodes = make([]string, len(bootClients))
		for i, bootClient := range bootClients {
			enodes[i], err = bootClient.EnodeURL()
			if err != nil {
				return nil, fmt.Errorf("Unable to obtain bootnode: %v", err)
			}
		}
	}

	if s.Config.MaxPeers == nil {
		s.Config.MaxPeers = DefaultMaxPeers
	}

	if s.ChainFile != "" {
		s.Config.ChainFile = "./chains/" + s.ChainFile
	} else {
		if chainFilePath, ok := ClientFiles["/chain.rlp"]; ok {
			s.Config.ChainFile = chainFilePath
		}
	}

	g, err := newNode(s.Config, enodes, &genesisCopy)
	if err != nil {
		return nil, err
	}

	go func(ctx context.Context) {
		select {
		case <-ctx.Done():
			// Close the node when the context is done
			if err := g.Close(); err != nil {
				panic(err)
			}
		}
	}(testContext)

	return g, nil
}

type AccountTransactionInfo struct {
	PreviousBlock common.Hash
	PreviousNonce uint64
}
type GethNode struct {
	node *node.Node
	eth  *eth.Ethereum

	mustHeadBlock *types.Header

	datadir    string
	genesis    *core.Genesis
	ttd        *big.Int
	api        *ethcatalyst.ConsensusAPI
	running    context.Context
	closing    context.CancelFunc
	closeMutex sync.Mutex

	// Engine updates info
	latestFcUStateSent *beacon.ForkchoiceStateV1
	latestPAttrSent    *beacon.PayloadAttributes
	latestFcUResponse  *beacon.ForkChoiceResponse

	latestPayloadSent          *beacon.ExecutableData
	latestPayloadStatusReponse *beacon.PayloadStatusV1

	// Test specific configuration
	accTxInfoMap                      map[common.Address]*AccountTransactionInfo
	totalReceivedOrAnnouncedNewBlocks *big.Int

	config GethNodeTestConfiguration
}

func newNode(config GethNodeTestConfiguration, bootnodes []string, genesis *core.Genesis) (*GethNode, error) {
	// Define the basic configurations for the Ethereum node
	datadir, _ := os.MkdirTemp("", "")

	return restart(config, bootnodes, datadir, genesis)
}

func restart(startConfig GethNodeTestConfiguration, bootnodes []string, datadir string, genesis *core.Genesis) (*GethNode, error) {
	if startConfig.Name == "" {
		startConfig.Name = "Modified Geth Module"
	}
	config := &node.Config{
		Name:    startConfig.Name,
		Version: params.Version,
		DataDir: datadir,
		P2P: p2p.Config{
			ListenAddr:  "0.0.0.0:0",
			NoDiscovery: true,
			MaxPeers:    int(startConfig.MaxPeers.Int64()),
		},
		UseLightweightKDF: true,
	}
	// Create the node and configure a full Ethereum node on it
	stack, err := node.New(config)
	if err != nil {
		return nil, err
	}
	if genesis == nil || genesis.Config == nil {
		return nil, fmt.Errorf("genesis configuration is nil")
	}
	if genesis.Config.TerminalTotalDifficultyPassed == false {
		return nil, fmt.Errorf("genesis configuration is has not passed terminal total difficulty")
	}
	econfig := &ethconfig.Config{
		Genesis:          genesis,
		NetworkId:        genesis.Config.ChainID.Uint64(),
		SyncMode:         downloader.FullSync,
		DatabaseCache:    256,
		DatabaseHandles:  256,
		TxPool:           ethconfig.Defaults.TxPool,
		GPO:              ethconfig.Defaults.GPO,
		Miner:            ethconfig.Defaults.Miner,
		LightServ:        100,
		LightPeers:       int(startConfig.MaxPeers.Int64()) - 1,
		LightNoSyncServe: true,
	}
	ethBackend, err := eth.New(stack, econfig)
	if err != nil {
		return nil, err
	}
	if startConfig.ChainFile != "" {
		err = utils.ImportChain(ethBackend.BlockChain(), startConfig.ChainFile)
		if err != nil {
			return nil, err
		}
	}

	err = stack.Start()
	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}
	// Connect the node to the bootnode
	if bootnodes != nil && len(bootnodes) > 0 {
		for _, bootnode := range bootnodes {
			node := enode.MustParse(bootnode)
			stack.Server().AddTrustedPeer(node)
			stack.Server().AddPeer(node)
		}
	}

	stack.Server().EnableMsgEvents = true

	g := &GethNode{
		node:         stack,
		eth:          ethBackend,
		datadir:      datadir,
		genesis:      genesis,
		ttd:          genesis.Config.TerminalTotalDifficulty,
		api:          ethcatalyst.NewConsensusAPI(ethBackend),
		accTxInfoMap: make(map[common.Address]*AccountTransactionInfo),
		// Test related configuration
		totalReceivedOrAnnouncedNewBlocks: big.NewInt(0),
		config:                            startConfig,
	}

	g.running, g.closing = context.WithCancel(context.Background())

	// Start thread to monitor the amount of gossiped blocks this node receives
	go g.SubscribeP2PEvents()

	return g, err
}

func (n *GethNode) AddPeer(ec client.EngineClient) error {
	bootnode, err := ec.EnodeURL()
	if err != nil {
		return err
	}
	node := enode.MustParse(bootnode)
	n.node.Server().AddTrustedPeer(node)
	n.node.Server().AddPeer(node)
	return nil
}

// Sets the canonical block to an ancestor of the provided block.
// `N==1` means to re-org to the parent of the provided block.
func (n *GethNode) ReOrgBackBlockChain(N uint64, currentBlock *types.Header) (*types.Header, error) {
	if N == 0 {
		return currentBlock, nil
	}
	for ; N > 0; N-- {
		currentBlock = n.eth.BlockChain().GetHeaderByHash(currentBlock.ParentHash)
		if currentBlock == nil {
			return nil, fmt.Errorf("Unable to re-org back")
		}
	}

	block := n.eth.BlockChain().GetBlock(currentBlock.Hash(), currentBlock.Number.Uint64())

	_, err := n.eth.BlockChain().SetCanonical(block)
	n.mustHeadBlock = currentBlock
	if err != nil {
		return nil, err
	}
	return currentBlock, nil
}

func (n *GethNode) SubscribeP2PEvents() {
	eventChan := make(chan *p2p.PeerEvent)
	subscription := n.node.Server().SubscribeEvents(eventChan)
	for {
		select {
		case event := <-eventChan:
			var msgCode uint64
			if event.MsgCode != nil {
				msgCode = *event.MsgCode
			}
			if event.Type == p2p.PeerEventTypeMsgRecv {
				if msgCode == ethprotocol.NewBlockMsg {
					n.totalReceivedOrAnnouncedNewBlocks.Add(n.totalReceivedOrAnnouncedNewBlocks, common.Big1)
					fmt.Printf("DEBUG (%s) [%v]: Received new block: Peer=%s, MsgCode=%d, Type=%s, Protocol=%s, RemoteAddress=%s\n", n.config.Name, time.Now(), event.Peer.String(), msgCode, event.Type, event.Protocol, event.RemoteAddress)
				} else if msgCode == ethprotocol.NewBlockHashesMsg {
					n.totalReceivedOrAnnouncedNewBlocks.Add(n.totalReceivedOrAnnouncedNewBlocks, common.Big1)
					fmt.Printf("DEBUG (%s) [%v]: Received new block announcement: Peer=%s, MsgCode=%d, Type=%s, Protocol=%s, RemoteAddress=%s\n", n.config.Name, time.Now(), event.Peer.String(), msgCode, event.Type, event.Protocol, event.RemoteAddress)
				}
			}

		case <-n.running.Done():
			subscription.Unsubscribe()
			return
		}
	}
}

func (n *GethNode) Close() error {
	n.closeMutex.Lock()
	defer n.closeMutex.Unlock()
	select {
	case <-n.running.Done():
		// Node already closed
		return nil
	default:
	}
	n.closing()
	err := n.node.Close()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(n.datadir); err != nil {
		panic(err)
	}
	return nil
}

type validator struct{}

func (v *validator) ValidateBody(block *types.Block) error {
	return nil
}
func (v *validator) ValidateState(block *types.Block, state *state.StateDB, receipts types.Receipts, usedGas uint64) error {
	return nil
}

type processor struct{}

func (p *processor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	return types.Receipts{}, []*types.Log{}, 21000, nil
}

var headerPrefix = []byte("h") // headerPrefix + num (uint64 big endian) + hash -> header
func headerKey(number uint64, hash common.Hash) []byte {
	return append(append(headerPrefix, encodeBlockNumber(number)...), hash.Bytes()...)
}

func encodeBlockNumber(number uint64) []byte {
	enc := make([]byte, 8)
	binary.BigEndian.PutUint64(enc, number)
	return enc
}

func (n *GethNode) SetBlock(block *types.Block, parentNumber uint64, parentRoot common.Hash) error {
	parentTd := n.eth.BlockChain().GetTd(block.ParentHash(), block.NumberU64()-1)
	rawdb.WriteTd(n.eth.ChainDb(), block.Hash(), block.NumberU64(), parentTd.Add(parentTd, block.Difficulty()))
	rawdb.WriteBlock(n.eth.ChainDb(), block)

	// write real info (fixes fake number test)
	data, err := rlp.EncodeToBytes(block.Header())
	if err != nil {
		log.Crit("Failed to RLP encode header", "err", err)
	}
	key := headerKey(parentNumber+1, block.Hash())
	if err := n.eth.ChainDb().Put(key, data); err != nil {
		log.Crit("Failed to store header", "err", err)
	}

	rawdb.WriteHeaderNumber(n.eth.ChainDb(), block.Hash(), block.NumberU64())
	bc := n.eth.BlockChain()
	bc.SetBlockValidatorAndProcessorForTesting(new(validator), n.eth.BlockChain().Processor())

	statedb, err := state.New(parentRoot, bc.StateCache(), bc.Snapshots())
	if err != nil {
		return err
	}
	statedb.StartPrefetcher("chain")
	var failedProcessing bool
	receipts, _, _, err := n.eth.BlockChain().Processor().Process(block, statedb, *n.eth.BlockChain().GetVMConfig())
	if err != nil {
		failedProcessing = true
	}
	rawdb.WriteReceipts(n.eth.ChainDb(), block.Hash(), block.NumberU64(), receipts)
	root, err := statedb.Commit(false)
	if err != nil {
		return err
	}

	triedb := bc.StateCache().TrieDB()
	if err := triedb.Commit(block.Root(), true); err != nil {
		return err
	}
	if err := triedb.Commit(root, true); err != nil {
		return err
	}

	rawdb.WriteHeadHeaderHash(n.eth.ChainDb(), block.Hash())
	rawdb.WriteHeadFastBlockHash(n.eth.ChainDb(), block.Hash())
	rawdb.WriteCanonicalHash(n.eth.ChainDb(), block.Hash(), block.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(n.eth.ChainDb(), block)
	rawdb.WriteHeadBlockHash(n.eth.ChainDb(), block.Hash())
	oldProcessor := bc.Processor()
	if failedProcessing {
		bc.SetBlockValidatorAndProcessorForTesting(new(validator), new(processor))
	}

	if _, err := bc.SetCanonical(block); err != nil {
		panic(err)
	}
	// Restore processor
	if failedProcessing {
		bc.SetBlockValidatorAndProcessorForTesting(new(validator), oldProcessor)
	}
	return nil
}

// Engine API

func (n *GethNode) NewPayload(ctx context.Context, version int, pl interface{}, versionedHashes []common.Hash) (beacon.PayloadStatusV1, error) {
	switch version {
	case 1:
		if c, ok := pl.(*client_types.ExecutableDataV1); ok {
			return n.NewPayloadV1(ctx, c)
		} else {
			return beacon.PayloadStatusV1{}, fmt.Errorf("wrong type %T", pl)
		}
	case 2:
		if c, ok := pl.(*beacon.ExecutableData); ok {
			return n.NewPayloadV2(ctx, c)
		} else {
			return beacon.PayloadStatusV1{}, fmt.Errorf("wrong type %T", pl)
		}
	}
	return beacon.PayloadStatusV1{}, fmt.Errorf("unknown version %d", version)
}

func (n *GethNode) NewPayloadV1(ctx context.Context, pl *client_types.ExecutableDataV1) (beacon.PayloadStatusV1, error) {
	ed := pl.ToExecutableData()
	n.latestPayloadSent = &ed
	resp, err := n.api.NewPayloadV1(ed)
	n.latestPayloadStatusReponse = &resp
	return resp, err
}

func (n *GethNode) NewPayloadV2(ctx context.Context, pl *beacon.ExecutableData) (beacon.PayloadStatusV1, error) {
	n.latestPayloadSent = pl
	resp, err := n.api.NewPayloadV2(*pl)
	n.latestPayloadStatusReponse = &resp
	return resp, err
}

func (n *GethNode) NewPayloadV3(ctx context.Context, pl *beacon.ExecutableData, versionedHashes []common.Hash) (beacon.PayloadStatusV1, error) {
	n.latestPayloadSent = pl
	resp, err := n.api.NewPayloadV3(*pl, &versionedHashes)
	n.latestPayloadStatusReponse = &resp
	return resp, err
}

func (n *GethNode) ForkchoiceUpdated(ctx context.Context, version int, fcs *beacon.ForkchoiceStateV1, payload *beacon.PayloadAttributes) (beacon.ForkChoiceResponse, error) {
	switch version {
	case 1:
		return n.ForkchoiceUpdatedV1(ctx, fcs, payload)
	case 2:
		return n.ForkchoiceUpdatedV2(ctx, fcs, payload)
	default:
		return beacon.ForkChoiceResponse{}, fmt.Errorf("unknown version %d", version)
	}
}

func (n *GethNode) ForkchoiceUpdatedV1(ctx context.Context, fcs *beacon.ForkchoiceStateV1, payload *beacon.PayloadAttributes) (beacon.ForkChoiceResponse, error) {
	n.latestFcUStateSent = fcs
	n.latestPAttrSent = payload
	fcr, err := n.api.ForkchoiceUpdatedV1(*fcs, payload)
	n.latestFcUResponse = &fcr
	return fcr, err
}

func (n *GethNode) ForkchoiceUpdatedV2(ctx context.Context, fcs *beacon.ForkchoiceStateV1, payload *beacon.PayloadAttributes) (beacon.ForkChoiceResponse, error) {
	n.latestFcUStateSent = fcs
	n.latestPAttrSent = payload
	fcr, err := n.api.ForkchoiceUpdatedV2(*fcs, payload)
	n.latestFcUResponse = &fcr
	return fcr, err
}

func (n *GethNode) GetPayloadV1(ctx context.Context, payloadId *beacon.PayloadID) (beacon.ExecutableData, error) {
	p, err := n.api.GetPayloadV1(*payloadId)
	if p == nil || err != nil {
		return beacon.ExecutableData{}, err
	}
	return *p, err
}

func (n *GethNode) GetPayloadV2(ctx context.Context, payloadId *beacon.PayloadID) (beacon.ExecutableData, *big.Int, error) {
	p, err := n.api.GetPayloadV2(*payloadId)
	if p == nil || err != nil {
		return beacon.ExecutableData{}, nil, err
	}
	return *p.ExecutionPayload, p.BlockValue, err
}

func (n *GethNode) GetPayloadBodiesByRangeV1(ctx context.Context, start uint64, count uint64) ([]*client_types.ExecutionPayloadBodyV1, error) {
	return nil, fmt.Errorf("not implemented")
}

func (n *GethNode) GetPayloadBodiesByHashV1(ctx context.Context, hashes []common.Hash) ([]*client_types.ExecutionPayloadBodyV1, error) {
	return nil, fmt.Errorf("not implemented")
}

// Eth JSON RPC

func parseBlockNumber(number *big.Int) rpc.BlockNumber {
	if number == nil {
		return rpc.LatestBlockNumber
	}
	return rpc.BlockNumber(number.Int64())
}

func (n *GethNode) BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error) {
	if number == nil && n.mustHeadBlock != nil {
		mustHeadHash := n.mustHeadBlock.Hash()
		block := n.eth.BlockChain().GetBlock(mustHeadHash, n.mustHeadBlock.Number.Uint64())
		return block, nil
	}
	return n.eth.APIBackend.BlockByNumber(ctx, parseBlockNumber(number))
}

func (n *GethNode) BlockNumber(ctx context.Context) (uint64, error) {
	if n.mustHeadBlock != nil {
		return n.mustHeadBlock.Number.Uint64(), nil
	}
	return n.eth.APIBackend.CurrentBlock().Number.Uint64(), nil
}

func (n *GethNode) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return n.eth.APIBackend.BlockByHash(ctx, hash)
}

func (n *GethNode) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	if number == nil && n.mustHeadBlock != nil {
		return n.mustHeadBlock, nil
	}
	b, err := n.eth.APIBackend.BlockByNumber(ctx, parseBlockNumber(number))
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("Block not found (%v)", number)
	}
	return b.Header(), err
}

func (n *GethNode) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	return n.eth.APIBackend.SendTx(ctx, tx)
}

func (n *GethNode) SendTransactions(ctx context.Context, txs []*types.Transaction) []error {
	for _, tx := range txs {
		err := n.eth.APIBackend.SendTx(ctx, tx)
		if err != nil {
			return []error{err}
		}
	}
	return nil
}

func (n *GethNode) getStateDB(ctx context.Context, blockNumber *big.Int) (*state.StateDB, error) {
	b, err := n.eth.APIBackend.BlockByNumber(ctx, parseBlockNumber(blockNumber))
	if err != nil {
		return nil, err
	}
	return state.New(b.Root(), n.eth.BlockChain().StateCache(), n.eth.BlockChain().Snapshots())
}

func (n *GethNode) BalanceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (*big.Int, error) {
	stateDB, err := n.getStateDB(ctx, blockNumber)
	if err != nil {
		return nil, err
	}
	return stateDB.GetBalance(account), nil
}

func (n *GethNode) StorageAt(ctx context.Context, account common.Address, key common.Hash, blockNumber *big.Int) ([]byte, error) {
	stateDB, err := n.getStateDB(ctx, blockNumber)
	if err != nil {
		return nil, err
	}
	return stateDB.GetState(account, key).Bytes(), nil
}

func (n *GethNode) StorageAtKeys(ctx context.Context, account common.Address, keys []common.Hash, blockNumber *big.Int) (map[common.Hash]*common.Hash, error) {
	return nil, fmt.Errorf("StorageAtKeys not implemented")
}

func (n *GethNode) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	return nil, fmt.Errorf("TransactionReceipt not implemented")
}

func (n *GethNode) NonceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (uint64, error) {
	stateDB, err := n.getStateDB(ctx, blockNumber)
	if err != nil {
		return 0, err
	}
	return stateDB.GetNonce(account), nil
}

func (n *GethNode) TransactionByHash(ctx context.Context, hash common.Hash) (tx *types.Transaction, isPending bool, err error) {
	panic("NOT IMPLEMENTED")
}

func (n *GethNode) GetBlockTotalDifficulty(ctx context.Context, hash common.Hash) (*big.Int, error) {
	block := n.eth.BlockChain().GetBlockByHash(hash)
	if block == nil {
		return big.NewInt(0), nil
	}
	return n.eth.BlockChain().GetTd(hash, block.NumberU64()), nil
}

func (n *GethNode) GetTotalDifficulty(ctx context.Context) (*big.Int, error) {
	if n.mustHeadBlock != nil {
		return n.GetBlockTotalDifficulty(ctx, n.mustHeadBlock.Hash())
	}
	return n.GetBlockTotalDifficulty(ctx, n.eth.BlockChain().CurrentHeader().Hash())
}

func (n *GethNode) TerminalTotalDifficulty() *big.Int {
	if n.ttd != nil {
		return n.ttd
	}
	return n.genesis.Config.TerminalTotalDifficulty
}

func (n *GethNode) EnodeURL() (string, error) {
	return n.node.Server().NodeInfo().Enode, nil
}

func (n *GethNode) ID() string {
	return n.node.Config().Name
}

func (n *GethNode) GetNextAccountNonce(testCtx context.Context, account common.Address) (uint64, error) {
	// First get the current head of the client where we will send the tx
	head, err := n.eth.APIBackend.BlockByNumber(testCtx, rpc.LatestBlockNumber)
	if err != nil {
		return 0, err
	}
	// Check if we have any info about this account, and when it was last updated
	if accTxInfo, ok := n.accTxInfoMap[account]; ok && accTxInfo != nil && (accTxInfo.PreviousBlock == head.Hash() || accTxInfo.PreviousBlock == head.ParentHash()) {
		// We have info about this account and is up to date (or up to date until the very last block).
		// Increase the nonce and return it
		accTxInfo.PreviousBlock = head.Hash()
		accTxInfo.PreviousNonce++
		return accTxInfo.PreviousNonce, nil
	}
	// We don't have info about this account, or is outdated, or we re-org'd, we must request the nonce
	nonce, err := n.NonceAt(testCtx, account, head.Number())
	if err != nil {
		return 0, err
	}
	n.accTxInfoMap[account] = &AccountTransactionInfo{
		PreviousBlock: head.Hash(),
		PreviousNonce: nonce,
	}
	return nonce, nil
}

func (n *GethNode) UpdateNonce(testCtx context.Context, account common.Address, newNonce uint64) error {
	// First get the current head of the client where we will send the tx
	head, err := n.eth.APIBackend.BlockByNumber(testCtx, rpc.LatestBlockNumber)
	if err != nil {
		return err
	}
	n.accTxInfoMap[account] = &AccountTransactionInfo{
		PreviousBlock: head.Hash(),
		PreviousNonce: newNonce,
	}
	return nil
}

func (n *GethNode) PostRunVerifications() error {
	// Check that the node did not receive more gossiped blocks than expected
	if n.config.ExpectedGossipNewBlocksCount != nil {
		if n.config.ExpectedGossipNewBlocksCount.Cmp(n.totalReceivedOrAnnouncedNewBlocks) != 0 {
			return fmt.Errorf("Node received gossiped blocks count different than expected: %d != %d", n.totalReceivedOrAnnouncedNewBlocks, n.config.ExpectedGossipNewBlocksCount)
		}
	}
	return nil
}

func (n *GethNode) LatestForkchoiceSent() (fcState *beacon.ForkchoiceStateV1, pAttributes *beacon.PayloadAttributes) {
	return n.latestFcUStateSent, n.latestPAttrSent
}

func (n *GethNode) LatestNewPayloadSent() (payload *beacon.ExecutableData) {
	return n.latestPayloadSent
}

func (n *GethNode) LatestForkchoiceResponse() (fcuResponse *beacon.ForkChoiceResponse) {
	return n.latestFcUResponse
}

func (n *GethNode) LatestNewPayloadResponse() (payloadResponse *beacon.PayloadStatusV1) {
	return n.latestPayloadStatusReponse
}
