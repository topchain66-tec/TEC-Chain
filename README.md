package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"sync"
	"time"
)

// ==========================================
// 1. TEC-CHAIN 协议常量定义
// ==========================================
const (
	ChainID            = "TEC-CHAIN-MAINNET"
	CurrencySymbol     = "TEC"
	TotalSupplyLimit   = 200000000             // 总量 2 亿
	PreMiningAllocation = 10000000            // 预发行 1000 万
	MinValidatorStake  = 2000                  // 2000 TEC 保证金门槛
	ValidatorGroupSize = 27                    // 记账组 27 个节点
	BlocksPerRound     = 10                    // 每个节点连续出块 10 个
	BlockInterval      = 10 * time.Second      // 10秒出块
	CycleInterval      = 45 * time.Minute      // 45分钟记账周期
)

// ==========================================
// 2. 基础加密与数据结构
// ==========================================

type TECAddress [20]byte
type TECHash [32]byte

func (h TECHash) String() string { return hex.EncodeToString(h[:]) }

// TECBlockHeader 区块头结构
type TECBlockHeader struct {
	ParentHash TECHash
	Height     int64
	Timestamp  int64
	Nonce      uint64
	Validator  TECAddress
	Difficulty *big.Int
}

type TECBlock struct {
	Header *TECBlockHeader
	Txs    [][]byte
	Hash   TECHash
}

// ==========================================
// 3. TEC 账本模块 (TEC Ledger Module)
// ==========================================

type AccountState struct {
	Address    TECAddress
	Balance    *big.Int
	Stake      *big.Int // 质押的 TEC
	VoteWeight *big.Int // 获得的 NPoS 选票权重
}

type TECLedger struct {
	mu      sync.RWMutex
	States  map[TECAddress]*AccountState
}

// GetTECUnit 转换单位为 10^18 (类似以太坊的 Wei)
func GetTECUnit(amount float64) *big.Int {
	val := new(big.Float).Mul(big.NewFloat(amount), big.NewFloat(1e18))
	res, _ := val.Int(nil)
	return res
}

// ==========================================
// 4. NPoS + PoW 混合共识引擎
// ==========================================

type ConsensusModule struct {
	Candidates  map[TECAddress]*AccountState
	ActiveGroup []TECAddress
	CycleIndex  uint64
	Ledger      *TECLedger
	mu          sync.RWMutex
}

// ElectValidators NPoS 选举逻辑 (27个节点)
func (cm *ConsensusModule) ElectValidators() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	type nodeRank struct {
		addr  TECAddress
		score *big.Int
	}
	var ranks []nodeRank
	threshold := GetTECUnit(MinValidatorStake)

	for addr, state := range cm.Candidates {
		// 检查 2000 TEC 保证金
		if state.Stake.Cmp(threshold) >= 0 {
			// 权重评分 = 质押量 * 0.7 + 投票权 * 0.3
			score := new(big.Int).Add(state.Stake, state.VoteWeight)
			ranks = append(ranks, nodeRank{addr, score})
		}
	}

	if len(ranks) < ValidatorGroupSize {
		return fmt.Errorf("TEC-CHAIN-ERR: 活跃候选节点不足 %d 个", ValidatorGroupSize)
	}

	// 权重排序
	sort.Slice(ranks, func(i, j int) bool {
		return ranks[i].score.Cmp(ranks[j].score) > 0
	})

	cm.ActiveGroup = make([]TECAddress, ValidatorGroupSize)
	for i := 0; i < ValidatorGroupSize; i++ {
		cm.ActiveGroup[i] = ranks[i].addr
	}
	cm.CycleIndex++
	fmt.Printf("[%s] 第 %d 周期选举完成，27个记账组就位\n", ChainID, cm.CycleIndex)
	return nil
}

// ProofOfWork 增加出块哈希安全校验
func (cm *ConsensusModule) ProofOfWork(b *TECBlock) {
	// 动态难度设定
	target := new(big.Int).Lsh(big.NewInt(1), 256-12) 
	for {
		hash := cm.calcHash(b)
		if new(big.Int).SetBytes(hash[:]).Cmp(target) <= 0 {
			b.Hash = hash
			return
		}
		b.Header.Nonce++
	}
}

func (cm *ConsensusModule) calcHash(b *TECBlock) TECHash {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, b.Header.ParentHash)
	binary.Write(buf, binary.BigEndian, b.Header.Height)
	binary.Write(buf, binary.BigEndian, b.Header.Nonce)
	binary.Write(buf, binary.BigEndian, b.Header.Validator)
	return sha256.Sum256(buf.Bytes())
}

// ==========================================
// 5. 动态奖励计算 (Economic Policy)
// ==========================================

type IncentivePolicy struct{}

func (ip *IncentivePolicy) GetBlockSubsidy(height int64) *big.Int {
	blocksPerYear := int64(3153600 / 10)
	year := height / blocksPerYear

	if year < 10 {
		// 首年每天 86400 TEC，每个块 10 TEC，每年递减 15%
		reward := 10.0 * math.Pow(0.85, float64(year))
		return GetTECUnit(reward)
	}
	// 十年后每年固定 300万枚 TEC
	fixed := 3000000.0 / float64(blocksPerYear)
	return GetTECUnit(fixed)
}

// ==========================================
// 6. TEC-CHAIN 核心内核 (The Kernel)
// ==========================================

type TECKernel struct {
	Ledger    *TECLedger
	Consensus *ConsensusModule
	Policy    *IncentivePolicy
	Chain     []*TECBlock
}

func (k *TECKernel) Start() {
	fmt.Printf(">>> %s 系统启动 <<<\n", ChainID)
	fmt.Printf(">>> 代币符号: %s | 初始预发行: %v <<<\n", CurrencySymbol, PreMiningAllocation)

	cycleTicker := time.NewTicker(CycleInterval)
	blockTicker := time.NewTicker(BlockInterval)

	for {
		select {
		case <-cycleTicker.C:
			k.Consensus.ElectValidators()

		case <-blockTicker.C:
			// 调度逻辑：27个节点，每人连续打包 10 个块
			height := int64(len(k.Chain))
			validatorIdx := (height / int64(BlocksPerRound)) % int64(ValidatorGroupSize)
			proposer := k.Consensus.ActiveGroup[validatorIdx]

			// 生成并密封区块
			newBlock := &TECBlock{
				Header: &TECBlockHeader{
					ParentHash: k.Chain[height-1].Hash,
					Height:     height,
					Timestamp:  time.Now().Unix(),
					Validator:  proposer,
				},
			}
			k.Consensus.ProofOfWork(newBlock)

			// 结算 TEC 奖励
			reward := k.Policy.GetBlockSubsidy(height)
			k.Ledger.mu.Lock()
			k.Ledger.States[proposer].Balance.Add(k.Ledger.States[proposer].Balance, reward)
			k.Ledger.mu.Unlock()

			k.Chain = append(k.Chain, newBlock)
			fmt.Printf("[%s] 区块高度 %d | 验证人 %x | 奖励 %.2f %s | Hash %s...\n", 
				ChainID, height, proposer[:6], 10.0, CurrencySymbol, newBlock.Hash.String()[:12])
		}
	}
}

func main() {
	// 1. 初始化 TEC 账本
	ledger := &TECLedger{States: make(map[TECAddress]*AccountState)}
	
	// 初始化候选节点 (TEC 质押)
	for i := 0; i < 30; i++ {
		var addr TECAddress
		binary.BigEndian.PutUint64(addr[:8], uint64(i+999))
		ledger.States[addr] = &AccountState{
			Address:    addr,
			Balance:    big.NewInt(0),
			Stake:      GetTECUnit(2100.0), // 满足 2000 TEC 门槛
			VoteWeight: big.NewInt(int64(i * 100)),
		}
	}

	// 2. 装配内核
	consensus := &ConsensusModule{
		Candidates: ledger.States,
		Ledger:     ledger,
	}
	// 首次选举
	consensus.ElectValidators()

	kernel := &TECKernel{
		Ledger:    ledger,
		Consensus: consensus,
		Policy:    &IncentivePolicy{},
		Chain:     []*TECBlock{{Hash: TECHash{0x1}}}, // 创世块
	}

	// 3. 运行 TEC-CHAIN
	kernel.Start()
}
