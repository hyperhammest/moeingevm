package ebp

import (
	"encoding/binary"
	"errors"
	//"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
	"github.com/smartbch/moeingads/store/rabbit"
	"github.com/seehuhn/mt19937"

	"github.com/smartbch/moeingevm/types"
	"github.com/smartbch/moeingevm/utils"
)

var (
	MaxTxGasLimit = 1000_0000
)

var _ TxExecutor = (*txEngine)(nil)

type TxRange struct {
	start uint64
	end   uint64
}

type txEngine struct {
	// How many parallel execution round are performed for each block
	roundNum int //consensus parameter
	// How many runners execute transactions in parallel for each round
	runnerNumber int //consensus parameter
	// The runners are driven by 'parallelNum' of goroutines
	parallelNum int //per-node parameter
	// A clean Context whose RabbitStore has no cache. It must be set before calling 'Execute', and
	// txEngine will close it at the end of 'Prepare'
	cleanCtx *types.Context
	// CollectTx fills txList and 'Prepare' handles and clears txList
	txList       []*gethtypes.Transaction
	committedTxs []*types.Transaction
	// Used to check signatures
	signer       gethtypes.Signer
	currentBlock *types.BlockInfo

	cumulativeGasUsed   uint64
	cumulativeGasRefund *uint256.Int
	cumulativeGasFee    *uint256.Int
}

func (exec *txEngine) Context() *types.Context {
	return exec.cleanCtx
}

// Generated by parallelReadAccounts and insertToStandbyTxQ will store its tx into world state.
type preparedInfo struct {
	tx        *types.TxToRun
	gasFee    *uint256.Int
	valid     bool
	statusStr string
}

// Generated by parallelReadAccounts and Prepare will use them for some tests.
type ctxAndAccounts struct {
	ctx         types.Context
	accounts    []common.Address
	changed     bool
	totalGasFee *uint256.Int
	addr2nonce  map[common.Address]uint64
}

func NewEbpTxExec(exeRoundCount, runnerNumber, parallelNum, defaultTxListCap int, s gethtypes.Signer) *txEngine {
	Runners = make([]*TxRunner, runnerNumber)
	return &txEngine{
		roundNum:     exeRoundCount,
		runnerNumber: runnerNumber,
		parallelNum:  parallelNum,
		txList:       make([]*gethtypes.Transaction, 0, defaultTxListCap),
		committedTxs: make([]*types.Transaction, 0, defaultTxListCap),
		signer:       s,
	}
}

// A new context must be set before Execute
func (exec *txEngine) SetContext(ctx *types.Context) {
	exec.cleanCtx = ctx
}

// Check transactions' signatures and insert the valid ones into standby queue
func (exec *txEngine) Prepare(reorderSeed int64, minGasPrice uint64) {
	if len(exec.txList) == 0 {
		exec.cleanCtx.Close(false)
		return
	}
	infoList, ctxAA := exec.parallelReadAccounts(minGasPrice)
	addr2idx := make(map[common.Address]int)      // map address to ctxAA's index
	for idx, entry := range ctxAA {
		for _, addr := range entry.accounts {
			if _, ok := addr2idx[addr]; !ok {
				//when we meet an address for the first time
				addr2idx[addr] = idx
			}
		}
	}
	reorderedList, addr2Infos := reorderInfoList(infoList, reorderSeed)
	parallelRun(exec.parallelNum, func(idx int) {
		entry := ctxAA[idx]
		for _, addr := range entry.accounts {
			if addr2idx[addr] != idx {
				// this addr does not belong to this entry
				continue
			}
			for _, info := range addr2Infos[addr] {
				if !info.valid {
					//skip it if account does not exist or signature is invalid
					continue
				}
				sender := info.tx.From
				if entry.addr2nonce[sender] != info.tx.Nonce {
					//skip it if nonce is wrong
					info.valid = false
					info.statusStr = "incorrect nonce"
					continue
				}
				entry.addr2nonce[sender]++
				err := SubSenderAccBalance(&entry.ctx, sender, info.gasFee)
				if err != nil {
					info.valid = false
					info.statusStr = "not enough balance to pay gasfee"
					continue
				} else {
					entry.totalGasFee.Add(entry.totalGasFee, info.gasFee)
				}
				entry.changed = true //now this rabbit needs writeback
			}
		}
	})
	for i := range ctxAA {
		ctxAA[i].ctx.Close(ctxAA[i].changed)
	}
	// the value of exec.parallelNum and the speeds of goroutines must have
	// no effects on the order of TXs in standby queue.
	ctx := exec.cleanCtx.WithRbtCopy()
	for i := range ctxAA {
		_ = AddSystemAccBalance(ctx, ctxAA[i].totalGasFee)
	}
	exec.insertToStandbyTxQ(ctx, reorderedList)
	exec.txList = exec.txList[:0] // clear txList after consumption
	//write ctx state to trunk
	ctx.Close(true)
	exec.cleanCtx.Close(false)
}

// Read accounts' information in parallel, while checking accouts' existence and signatures' validity
func (exec *txEngine) parallelReadAccounts(minGasPrice uint64) (infoList []preparedInfo, ctxAA []ctxAndAccounts) {
	//for each tx, we fetch some info for it
	infoList = make([]*preparedInfo, len(exec.txList))
	//the ctx and accounts that a worker works at
	ctxAA = make([]*ctxAndAccounts, exec.parallelNum)
	sharedIdx := int64(-1)
	estimatedSize := len(exec.txList)/exec.parallelNum + 1
	parallelRun(exec.parallelNum, func(workerId int) {
		ctxAA[workerId] = &ctxAndAccounts{
			ctx: *exec.cleanCtx.WithRbtCopy(),
			accounts: make([]common.Address, 0, estimatedSize),
			changed: false,
			totalGasFee: uint256.NewInt(),
			addr2nonce: make(map[common.Address]uint64),
		}
		for {
			myIdx := atomic.AddInt64(&sharedIdx, 1)
			if int(myIdx) >= len(exec.txList) {
				return
			}
			tx := exec.txList[myIdx]
			infoList[myIdx] = &preparedInfo{}
			// we need some computation to get the sender's address
			sender, err := exec.signer.Sender(tx)
			//set txToRun first
			txToRun := &types.TxToRun{}
			txToRun.FromGethTx(tx, sender, exec.cleanCtx.Height)
			infoList[myIdx].tx = txToRun
			if err != nil {
				infoList[myIdx].valid = false
				infoList[myIdx].statusStr = "invalid signature"
				continue // skip invalid signature
			}
			//todo: check if overflow or not
			gasPrice, _ := uint256.FromBig(tx.GasPrice())
			if gasPrice.Cmp(uint256.NewInt().SetUint64(minGasPrice)) < 0 {
				infoList[myIdx].valid = false
				infoList[myIdx].statusStr = "invalid gas price"
				continue // skip invalid tx gas price
			}
			if tx.Gas() > uint64(MaxTxGasLimit) {
				infoList[myIdx].valid = false
				infoList[myIdx].statusStr = "invalid gas limit"
				continue // skip invalid tx gas limit
			}
      // access disk to fetch the account's detail
			acc := ctxAA[workerId].ctx.GetAccount(sender)
			infoList[myIdx].valid = acc != nil
			if acc == nil {
				infoList[myIdx].statusStr = "non-existent account"
				continue // skip non-existent account
			}
			if _, ok := ctxAA[workerId].addr2nonce[sender]; !ok {
				ctxAA[workerId].accounts = append(ctxAA[workerId].accounts, sender)
				ctxAA[workerId].addr2nonce[sender] = acc.Nonce()
			}
			infoList[myIdx].tx = txToRun
			gasFee := uint256.NewInt().SetUint64(txToRun.Gas)
			infoList[myIdx].gasFee = gasFee.Mul(gasFee, utils.U256FromSlice32(txToRun.GasPrice[:]))
		}
	})
	return
}

func reorderInfoList(infoList []*preparedInfo, reorderSeed int64) (out []*preparedInfo, addr2Infos map[common.Address][]*preparedInfo) {
	out = make([]*preparedInfo, 0, len(infoList))
	addr2Infos = make(map[common.Address][]*preparedInfo, len(infoList))
	addrList := make([]common.Address, 0, len(infoList))
	for _, info := range infoList {
		if _, ok := addr2Infos[info.tx.From]; ok {
			addr2Infos[info.tx.From] = append(addr2Infos[info.tx.From], info)
		} else {
			addr2Infos[info.tx.From] = []*preparedInfo{info}
			addrList = append(addrList, info.tx.From)
		}
	}
	rand := mt19937.New()
	rand.Seed(reorderSeed)
	for i := 0; i < len(addrList); i++ {
		r0 := int(rand.Int63()) % len(addrList)
		r1 := int(rand.Int63()) % len(addrList)
		addrList[r0], addrList[r1] = addrList[r1], addrList[r0]
	}
	for _, addr := range addrList {
		for _, info := range addr2Infos[addr] {
			out = append(out, info)
		}
	}
	return
}

// insert valid transactions into standby queue
func (exec *txEngine) insertToStandbyTxQ(ctx *types.Context, infoList []*preparedInfo) {
	rbt := ctx.Rbt
	startEnd := rbt.Get(types.StandbyTxQueueKey)
	end := uint64(0)
	if len(startEnd) >= 8 {
		end = binary.BigEndian.Uint64(startEnd[8:])
	} else {
		startEnd = make([]byte, 16)
	}
	for _, info := range infoList {
		if !info.valid {
			exec.recordInvalidTx(info)
			continue
		}
		k := types.GetStandbyTxKey(end)
		rbt.Set(k, info.tx.ToBytes())
		end++
	}
	binary.BigEndian.PutUint64(startEnd[8:], end)
	rbt.Set(types.StandbyTxQueueKey, startEnd) //update start&end pointers of standby queue
}

func (exec *txEngine) recordInvalidTx(info *preparedInfo) {
	tx := &types.Transaction{
		Hash:              info.tx.HashID,
		TransactionIndex:  int64(len(exec.committedTxs)),
		Nonce:             info.tx.Nonce,
		BlockNumber:       int64(exec.cleanCtx.Height),
		From:              info.tx.From,
		To:                info.tx.To,
		Value:             info.tx.Value,
		GasPrice:          info.tx.GasPrice,
		Gas:               info.tx.Gas,
		Input:             info.tx.Data,
		CumulativeGasUsed: exec.cumulativeGasUsed,
		GasUsed:           0,
		Status:            gethtypes.ReceiptStatusFailed,
		StatusStr:         info.statusStr,
	}
	if exec.currentBlock != nil {
		tx.BlockHash = exec.currentBlock.Hash
	}
	exec.committedTxs = append(exec.committedTxs, tx)
}

// Fetch TXs from standby queue and execute them
func (exec *txEngine) Execute(currBlock *types.BlockInfo) {
	exec.committedTxs = exec.committedTxs[:0]
	exec.cumulativeGasUsed = 0
	exec.cumulativeGasRefund = uint256.NewInt().SetUint64(0)
	exec.cumulativeGasFee = uint256.NewInt().SetUint64(0)
	exec.currentBlock = currBlock
	startKey, endKey := exec.getStandbyQueueRange()
	if startKey == endKey {
		//fmt.Println("::::DEBUG: no transaction to execute in ExecuteNRound")
		return
	}
	txRange := &TxRange{
		start: startKey,
		end:   endKey,
	}
	committableTxList := make([]*TxRunner, 0, 4096)
	// Repeat exec.roundNum round for execute txs in standby q. At the end of each round
	// modifications made by TXs are written to world state. So TXs in later rounds will
	// be affected by the modifications made by TXs in earlier rounds.
	for i := 0; i < exec.roundNum; i++ {
		if txRange.start == txRange.end {
			break
		}
		numTx := exec.executeOneRound(txRange, exec.currentBlock)
		//var tmp = make([]*TxRunner, 0, 4096)
		for i := 0; i < numTx; i++ {
			if Runners[i] == nil {
				continue
			}
			committableTxList = append(committableTxList, Runners[i])
			//tmp = append(tmp, Runners[i])
			Runners[i] = nil
		}
		//for _, runner := range tmp {
		//			fmt.Printf(`
		//from:%s
		//to:%s
		//nonce:%d
		//value :%d
		//`, runner.Tx.From.String(), runner.Tx.To.String(), runner.Tx.Nonce, runner.Tx.Value)
		//}
	}
	exec.setStandbyQueueRange(txRange.start, txRange.end)
	exec.collectCommittableTxs(committableTxList)
	return
}

// Get the start and end position of standby queue
func (exec *txEngine) getStandbyQueueRange() (start, end uint64) {
	ctx := exec.cleanCtx.WithRbtCopy()
	defer ctx.Close(false)
	startEnd := ctx.Rbt.Get(types.StandbyTxQueueKey)
	if startEnd == nil {
		return 0, 0
	}
	return binary.BigEndian.Uint64(startEnd[:8]), binary.BigEndian.Uint64(startEnd[8:])
}

// Set the start and end position of standby queue
func (exec *txEngine) setStandbyQueueRange(start, end uint64) {
	ctx := exec.cleanCtx.WithRbtCopy()
	startEnd := make([]byte, 16)
	binary.BigEndian.PutUint64(startEnd[:8], start)
	binary.BigEndian.PutUint64(startEnd[8:], end)
	ctx.Rbt.Set(types.StandbyTxQueueKey, startEnd)
	ctx.Close(true)
}

// Execute 'runnerNumber' transactions in parallel and commit the ones without any interdependency
func (exec *txEngine) executeOneRound(txRange *TxRange, currBlock *types.BlockInfo) int {
	standbyTxList := exec.loadStandbyTxs(txRange)
	exec.runTxInParallel(standbyTxList, currBlock)
	exec.checkTxDepsAndUptStandbyQ(txRange, standbyTxList)
	return len(standbyTxList)
}

// Load at most 'exec.runnerNumber' transactions from standby queue
func (exec *txEngine) loadStandbyTxs(txRange *TxRange) (txBundle []types.TxToRun) {
	ctx := exec.cleanCtx.WithRbtCopy()
	end := txRange.end
	if end > txRange.start+uint64(exec.runnerNumber) {
		end = txRange.start + uint64(exec.runnerNumber)
	}
	txBundle = make([]types.TxToRun, end-txRange.start)
	for i := txRange.start; i < end; i++ {
		k := types.GetStandbyTxKey(i)
		bz := ctx.Rbt.Get(k)
		txBundle[i-txRange.start].FromBytes(bz)
	}
	ctx.Close(false)
	return
}

// Assign the transactions to global 'Runners' and run them in parallel
func (exec *txEngine) runTxInParallel(txBundle []types.TxToRun, currBlock *types.BlockInfo) {
	sharedIdx := int64(-1)
	parallelRun(exec.parallelNum, func(_ int) {
		for {
			myIdx := atomic.AddInt64(&sharedIdx, 1)
			if myIdx >= int64(len(txBundle)) {
				return
			}
			Runners[myIdx] = &TxRunner{
				id:  myIdx,
				Ctx: *exec.cleanCtx.WithRbtCopy(),
				Tx:  &txBundle[myIdx],
			}
			runTx(int(myIdx), currBlock)
		}
	})
}

// Check interdependency of TXs using 'touchedSet'. The ones with dependency with former committed TXs cannot
// be committed and should be inserted back into the standby queue.
// A TX whose nonce is too small should also be inserted back into the standby queue.
func (exec *txEngine) checkTxDepsAndUptStandbyQ(txRange *TxRange, standbyTxList []types.TxToRun) {
	touchedSet := make(map[uint64]struct{}, 1000)
	for idx := range standbyTxList {
		canCommit := true
		Runners[idx].Ctx.Rbt.ScanAllShortKeys(func(key [rabbit.KeySize]byte, dirty bool) (stop bool) {
			k := binary.LittleEndian.Uint64(key[:])
			if _, ok := touchedSet[k]; ok {
				canCommit = false // cannot commit if conflicts with touched KV set
				Runners[idx].Status = types.FAILED_TO_COMMIT
				return true
			} else {
				return false
			}
		})
		if canCommit { // record the dirty KVs written by a committable TX into toucchedSet
			Runners[idx].Ctx.Rbt.ScanAllShortKeys(func(key [rabbit.KeySize]byte, dirty bool) (stop bool) {
				if dirty {
					k := binary.LittleEndian.Uint64(key[:])
					touchedSet[k] = struct{}{}
				}
				return false
			})
		}
		Runners[idx].Ctx.Rbt.CloseAndWriteBack(canCommit)
	}

	ctx := exec.cleanCtx.WithRbtCopy()
	for idx, tx := range standbyTxList {
		k := types.GetStandbyTxKey(txRange.start)
		txRange.start++
		ctx.Rbt.Delete(k) // remove it from the standby queue
		status := Runners[idx].Status
		if status == types.FAILED_TO_COMMIT || status == types.TX_NONCE_TOO_LARGE {
			newK := types.GetStandbyTxKey(txRange.end)
			txRange.end++
			ctx.Rbt.Set(newK, tx.ToBytes()) // insert the failed TXs back into standby queue
			Runners[idx] = nil
		} else if status == types.ACCOUNT_NOT_EXIST || status == types.TX_NONCE_TOO_SMALL {
			//collect invalid tx`s all gas
			exec.cumulativeGasUsed += Runners[idx].Tx.Gas
			Runners[idx] = nil
		}
	}
	ctx.Close(true)
}

// Fill 'exec.committedTxs' with 'committableTxList'
func (exec *txEngine) collectCommittableTxs(committableTxList []*TxRunner) {
	var logIndex uint
	for idx, runner := range committableTxList {
		exec.cumulativeGasUsed += runner.GasUsed
		exec.cumulativeGasRefund.Add(exec.cumulativeGasRefund, &runner.GasRefund)
		exec.cumulativeGasFee.Add(exec.cumulativeGasFee,
			uint256.NewInt().Mul(uint256.NewInt().SetUint64(runner.GasUsed),
				uint256.NewInt().SetBytes(runner.Tx.GasPrice[:])))
		tx := &types.Transaction{
			Hash:              runner.Tx.HashID,
			TransactionIndex:  int64(idx),
			Nonce:             runner.Tx.Nonce,
			BlockHash:         exec.currentBlock.Hash,
			BlockNumber:       int64(exec.cleanCtx.Height),
			From:              runner.Tx.From,
			To:                runner.Tx.To,
			Value:             runner.Tx.Value,
			GasPrice:          runner.Tx.GasPrice,
			Gas:               runner.Tx.Gas,
			Input:             runner.Tx.Data,
			CumulativeGasUsed: exec.cumulativeGasUsed,
			GasUsed:           runner.GasUsed,
			ContractAddress:   runner.CreatedContractAddress, //20 Bytes - the contract address created, if the transaction was a contract creation, otherwise - null. TODO testme!
			OutData:           append([]byte{}, runner.OutData...),
			Status:            gethtypes.ReceiptStatusSuccessful,
			StatusStr:         StatusToStr(runner.Status),
		}
		if StatusIsFailure(runner.Status) {
			tx.Status = gethtypes.ReceiptStatusFailed
		}
		tx.Logs = make([]types.Log, len(runner.Logs))
		for i, log := range runner.Logs {
			copy(tx.Logs[i].Address[:], log.Address[:])
			tx.Logs[i].Topics = make([][32]byte, len(log.Topics))
			for j, t := range log.Topics {
				copy(tx.Logs[i].Topics[j][:], t[:])
			}
			tx.Logs[i].Data = log.Data
			log.Data = nil
			tx.Logs[i].BlockNumber = uint64(exec.currentBlock.Number)
			copy(tx.Logs[i].BlockHash[:], exec.currentBlock.Hash[:])
			copy(tx.Logs[i].TxHash[:], tx.Hash[:])
			//txIndex = index in committableTxList
			tx.Logs[i].TxIndex = uint(idx)
			tx.Logs[i].Index = logIndex
			logIndex++
			tx.Logs[i].Removed = false
		}
		tx.LogsBloom = LogsBloom(tx.Logs)
		exec.committedTxs = append(exec.committedTxs, tx)
	}
}

func LogsBloom(logs []types.Log) [256]byte {
	var bin gethtypes.Bloom
	for _, log := range logs {
		bin.Add(log.Address[:])
		for _, b := range log.Topics {
			bin.Add(b[:])
		}
	}
	return bin
}

func (exec *txEngine) CommittedTxs() []*types.Transaction {
	return exec.committedTxs
}

func (exec *txEngine) CollectTx(tx *gethtypes.Transaction) {
	exec.txList = append(exec.txList, tx)
}

func (exec *txEngine) CollectTxsCount() int {
	return len(exec.txList)
}

func (exec *txEngine) GasUsedInfo() (gasUsed uint64, gasRefund, gasFee uint256.Int) {
	if exec.cumulativeGasFee == nil {
		return exec.cumulativeGasUsed, *exec.cumulativeGasRefund, uint256.Int{}
	}
	return exec.cumulativeGasUsed, *exec.cumulativeGasRefund, *exec.cumulativeGasFee
}

func (exec *txEngine) StandbyQLen() int {
	s, e := exec.getStandbyQueueRange()
	return int(e - s)
}

func parallelRun(workerCount int, fn func(workerID int)) {
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func(i int) {
			fn(i)
			wg.Done()
		}(i)
	}
	wg.Wait()
}

var (
	// record pending gas fee and refund
	systemContractAddress = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		byte('s'), byte('y'), byte('s'), byte('t'), byte('e'), byte('m')}
	// record distribute fee
	blackHoleContractAddress = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		byte('b'), byte('l'), byte('a'), byte('c'), byte('k'), byte('h'), byte('o'), byte('l'), byte('e')}
)

// guarantee account exists externally
func SubSenderAccBalance(ctx *types.Context, sender common.Address, amount *uint256.Int) error {
	return updateBalance(ctx, sender, amount, false)
}

func AddSystemAccBalance(ctx *types.Context, amount *uint256.Int) error {
	return updateBalance(ctx, systemContractAddress, amount, true)
}

func SubSystemAccBalance(ctx *types.Context, amount *uint256.Int) error {
	return updateBalance(ctx, systemContractAddress, amount, false)
}

func TransferFromSenderAccToBlackHoleAcc(ctx *types.Context, sender common.Address, amount *uint256.Int) error {
	err := updateBalance(ctx, sender, amount, false)
	if err != nil {
		return err
	}
	return updateBalance(ctx, blackHoleContractAddress, amount, true)
}

// will lazy init account if acc not exist
func updateBalance(ctx *types.Context, address common.Address, amount *uint256.Int, isAdd bool) error {
	acc := ctx.GetAccount(address)
	if acc == nil {
		//lazy init
		acc = types.ZeroAccountInfo()
	}
	s := acc.Balance()
	if isAdd {
		acc.UpdateBalance(s.Add(s, amount))
	} else {
		if s.Cmp(amount) < 0 {
			return errors.New("balance not enough")
		}
		acc.UpdateBalance(s.Sub(s, amount))
	}
	ctx.SetAccount(address, acc)
	return nil
}

func GetSystemBalance(ctx *types.Context) *uint256.Int {
	acc := ctx.GetAccount(systemContractAddress)
	if acc == nil {
		acc = types.ZeroAccountInfo()
	}
	return acc.Balance()
}

func GetBlackHoleBalance(ctx *types.Context) *uint256.Int {
	acc := ctx.GetAccount(blackHoleContractAddress)
	if acc == nil {
		acc = types.ZeroAccountInfo()
	}
	return acc.Balance()
}
