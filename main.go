package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
)

// ====================== Core Types ======================
type Address [20]byte
type Hash [32]byte

func (a Address) String() string {
	return "TEC" + hex.EncodeToString(a[:])[:38]
}

func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

type BlockHeader struct {
	ParentHash Hash
	Height     int64
	Timestamp  int64
	Nonce      uint64
	Validator  Address
	Difficulty *big.Int
}

type Block struct {
	Header *BlockHeader
	Txs    []Transaction
	Hash   Hash
}

type Transaction struct {
	From      Address
	To        Address
	Amount    *big.Int
	Fee       *big.Int
	Nonce     uint64
	Signature []byte
	Hash      Hash
}

func (tx Transaction) CalculateHash() Hash {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, tx.From[:])
	binary.Write(buf, binary.BigEndian, tx.To[:])
	binary.Write(buf, binary.BigEndian, tx.Amount.Bytes())
	binary.Write(buf, binary.BigEndian, tx.Fee.Bytes())
	binary.Write(buf, binary.BigEndian, tx.Nonce)
	return sha256.Sum256(buf.Bytes())
}

// ====================== Ledger ======================
type AccountState struct {
	Balance    *big.Int
	Stake      *big.Int
	VoteWeight *big.Int
	Nonce      uint64
}

type Ledger struct {
	mu     sync.RWMutex
	States map[Address]*AccountState
}

func NewLedger() *Ledger {
	l := &Ledger{States: make(map[Address]*AccountState)}
	return l
}

func (l *Ledger) ApplyTransaction(tx Transaction) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	sender, exists := l.States[tx.From]
	if !exists || sender.Nonce != tx.Nonce {
		return errors.New("invalid sender or nonce")
	}

	total := new(big.Int).Add(tx.Amount, tx.Fee)
	if sender.Balance.Cmp(total) < 0 {
		return errors.New("insufficient balance")
	}

	sender.Balance.Sub(sender.Balance, total)
	sender.Nonce++

	if _, ok := l.States[tx.To]; !ok {
		l.States[tx.To] = &AccountState{Balance: big.NewInt(0), Stake: big.NewInt(0)}
	}
	l.States[tx.To].Balance.Add(l.States[tx.To].Balance, tx.Amount)

	return nil
}

// ====================== Mempool ======================
type Mempool struct {
	txs map[Hash]Transaction
	mu  sync.RWMutex
}

func NewMempool() *Mempool {
	return &Mempool{txs: make(map[Hash]Transaction)}
}

func (m *Mempool) Add(tx Transaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	hash := tx.CalculateHash()
	m.txs[hash] = tx
	return nil
}

func (m *Mempool) Pending() []Transaction {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []Transaction
	for _, tx := range m.txs {
		list = append(list, tx)
	}
	return list
}

func (m *Mempool) Clear(txs []Transaction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tx := range txs {
		delete(m.txs, tx.Hash)
	}
}

// ====================== Consensus ======================
type Consensus struct {
	ActiveValidators []Address
	mu               sync.RWMutex
}

func (c *Consensus) ElectValidators(states map[Address]*AccountState) {
	c.mu.Lock()
	defer c.mu.Unlock()

	type ranked struct {
		addr  Address
		score *big.Int
	}

	var ranks []ranked
	for addr, state := range states {
		if state.Stake.Cmp(big.NewInt(2000*1e18)) >= 0 {
			score := new(big.Int).Mul(state.Stake, big.NewInt(70))
			score.Add(score, new(big.Int).Mul(state.VoteWeight, big.NewInt(30)))
			score.Div(score, big.NewInt(100))
			ranks = append(ranks, ranked{addr, score})
		}
	}

	sort.Slice(ranks, func(i, j int) bool {
		return ranks[i].score.Cmp(ranks[j].score) > 0
	})

	c.ActiveValidators = make([]Address, 0, 27)
	for i := 0; i < 27 && i < len(ranks); i++ {
		c.ActiveValidators = append(c.ActiveValidators, ranks[i].addr)
	}
}

func (c *Consensus) GetValidator(height int64) Address {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.ActiveValidators) == 0 {
		return Address{}
	}
	idx := (height / 10) % int64(len(c.ActiveValidators))
	return c.ActiveValidators[idx]
}

// ====================== Node ======================
type Node struct {
	Ledger     *Ledger
	Mempool    *Mempool
	Consensus  *Consensus
	Chain      []*Block
	mu         sync.RWMutex
	shutdown   chan struct{}
}

func NewNode() *Node {
	ledger := NewLedger()

	// Create test validators
	for i := 0; i < 35; i++ {
		var addr Address
		binary.BigEndian.PutUint64(addr[:], uint64(1000+i))
		ledger.States[addr] = &AccountState{
			Balance:    big.NewInt(500000 * 1e18),
			Stake:      big.NewInt(5000 * 1e18),
			VoteWeight: big.NewInt(int64(i * 50)),
		}
	}

	consensus := &Consensus{}
	consensus.ElectValidators(ledger.States)

	node := &Node{
		Ledger:    ledger,
		Mempool:   NewMempool(),
		Consensus: consensus,
		Chain:     make([]*Block, 0),
		shutdown:  make(chan struct{}),
	}

	// Genesis Block
	genesis := &Block{
		Header: &BlockHeader{
			Height:    0,
			Timestamp: time.Now().Unix(),
		},
	}
	genesis.Hash = Hash(sha256.Sum256([]byte("tec-genesis-2026")))
	node.Chain = append(node.Chain, genesis)

	return node
}

func (n *Node) Start() {
	ticker := time.NewTicker(12 * time.Second)
	epochTicker := time.NewTicker(90 * time.Second)

	for {
		select {
		case <-n.shutdown:
			return
		case <-ticker.C:
			n.produceBlock()
		case <-epochTicker.C:
			n.Consensus.ElectValidators(n.Ledger.States)
			fmt.Println("🔄 New validator set elected")
		}
	}
}

func (n *Node) produceBlock() {
	n.mu.Lock()
	defer n.mu.Unlock()

	height := int64(len(n.Chain))
	validator := n.Consensus.GetValidator(height)

	block := &Block{
		Header: &BlockHeader{
			ParentHash: n.Chain[len(n.Chain)-1].Hash,
			Height:     height,
			Timestamp:  time.Now().Unix(),
			Validator:  validator,
		},
		Txs: n.Mempool.Pending()[:5],
	}

	// Simple PoW
	block.Header.Nonce = uint64(time.Now().Unix())

	// Apply transactions
	for _, tx := range block.Txs {
		if err := n.Ledger.ApplyTransaction(tx); err != nil {
			fmt.Printf("Tx failed: %v\n", err)
		}
	}

	// Block reward
	if acc, ok := n.Ledger.States[validator]; ok {
		acc.Balance.Add(acc.Balance, big.NewInt(12*1e18))
	}

	block.Hash = Hash(sha256.Sum256([]byte(fmt.Sprintf("block-%d", height))))
	n.Chain = append(n.Chain, block)
	n.Mempool.Clear(block.Txs)

	fmt.Printf("✅ Block #%d mined by %s | Txs: %d\n", height, validator.String()[:12], len(block.Txs))
}

// ====================== RPC ======================
func (n *Node) StartRPC() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
			ID     interface{}   `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		response := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
		}

		switch req.Method {
		case "tec_getBlockByNumber":
			response["result"] = map[string]int{"height": len(n.Chain) - 1}
		case "tec_getBalance":
			response["result"] = "500000000000000000000"
		case "tec_sendRawTransaction":
			response["result"] = "0x" + hex.EncodeToString([]byte("mocktxhash"))
		default:
			response["error"] = map[string]string{"message": "Method not supported"}
		}

		json.NewEncoder(w).Encode(response)
	})

	fmt.Println("📡 JSON-RPC server listening on http://localhost:8545")
	log.Fatal(http.ListenAndServe(":8545", nil))
}

func (n *Node) Stop() {
	close(n.shutdown)
}

// ====================== Main ======================
func main() {
	node := NewNode()

	go node.StartRPC()
	go node.Start()

	fmt.Println("🚀 TEC-CHAIN Blockchain Node Started")
	fmt.Println("📡 JSON-RPC: http://localhost:8545")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	node.Stop()
	fmt.Println("👋 TEC-CHAIN shutdown complete")
}
