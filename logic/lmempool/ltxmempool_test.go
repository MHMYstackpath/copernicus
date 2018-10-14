package lmempool_test

import (
	"encoding/hex"
	"sync"
	"time"

	"github.com/copernet/copernicus/conf"
	"github.com/copernet/copernicus/crypto"
	"github.com/copernet/copernicus/errcode"
	"github.com/copernet/copernicus/logic/lmempool"
	"github.com/copernet/copernicus/logic/ltx"
	"github.com/copernet/copernicus/model"
	mmempool "github.com/copernet/copernicus/model/mempool"
	"github.com/copernet/copernicus/model/outpoint"
	"github.com/copernet/copernicus/model/script"
	"github.com/copernet/copernicus/model/tx"
	"github.com/copernet/copernicus/model/txin"
	"github.com/copernet/copernicus/model/txout"
	"github.com/copernet/copernicus/model/utxo"
	"github.com/copernet/copernicus/persist/db"
	"github.com/copernet/copernicus/service"
	"github.com/copernet/copernicus/util"
	"github.com/copernet/copernicus/util/amount"
	"github.com/copernet/copernicus/util/cashaddr"
	"math"
	"math/rand"
	"os"
	"runtime"
	"testing"
)

func init() {
	crypto.InitSecp256()
}

type fakeChain struct {
	sync.RWMutex
	utxos          *utxo.CoinsMap
	currentHeight  int32
	medianTimePast time.Time
}

func (s *fakeChain) FetchUtxoView(tx *tx.Tx) (*utxo.CoinsMap, error) {
	s.RLock()
	defer s.RUnlock()

	viewpoint := utxo.NewEmptyCoinsMap()
	prevOut := &outpoint.OutPoint{Hash: tx.GetHash()}
	for txOutIdx := 0; txOutIdx < tx.GetOutsCount(); txOutIdx++ {
		prevOut.Index = uint32(txOutIdx)
		entry := s.utxos.GetCoin(prevOut)
		viewpoint.GetMap()[*prevOut] = entry.DeepCopy()
	}

	for txInIdx := 0; txInIdx < tx.GetInsCount(); txInIdx++ {
		txIn := tx.GetTxIn(txInIdx)
		entry := s.utxos.GetCoin(txIn.PreviousOutPoint)
		viewpoint.GetMap()[*txIn.PreviousOutPoint] = entry.DeepCopy()
	}

	return viewpoint, nil
}

func (s *fakeChain) BestHeight() int32 {
	s.RLock()
	height := s.currentHeight
	s.RUnlock()
	return height
}

func (s *fakeChain) SetHeight(height int32) {
	s.Lock()
	s.currentHeight = height
	s.Unlock()
}

func (s *fakeChain) MedianTimePast() time.Time {
	s.RLock()
	mtp := s.medianTimePast
	s.RUnlock()
	return mtp
}

// SetMedianTimePast sets the current median time past associated with the fake
// chain instance.
func (s *fakeChain) SetMedianTimePast(mtp time.Time) {
	s.Lock()
	s.medianTimePast = mtp
	s.Unlock()
}

// CalcSequenceLock returns the current sequence lock for the passed
// transaction associated with the fake chain instance.
// func (s *fakeChain) CalcSequenceLock(tx *tx.Tx,
// 	view *utxo.CoinsMap) (*blockchain.SequenceLock, error) {

// 	return &blockchain.SequenceLock{
// 		Seconds:     -1,
// 		BlockHeight: -1,
// 	}, nil
// }

// spendableOutput is a convenience type that houses a particular utxo and the
// amount associated with it.
type spendableOutput struct {
	outPoint outpoint.OutPoint
	amount   amount.Amount
}

// txOutToSpendableOut returns a spendable output given a transaction and index
// of the output to use.  This is useful as a convenience when creating test
// transactions.
func txOutToSpendableOut(tx *tx.Tx, outputNum uint32) spendableOutput {
	return spendableOutput{
		outPoint: outpoint.OutPoint{Hash: tx.GetHash(), Index: outputNum},
		amount:   amount.Amount(tx.GetTxOut(int(outputNum)).GetValue()),
	}
}

// poolHarness provides a harness that includes functionality for creating and
// signing transactions as well as a fake chain that provides utxos for use in
// generating valid transactions.
type poolHarness struct {
	// signKey is the signing key used for creating transactions throughout
	// the tests.
	//
	// payAddr is the p2sh address for the signing key and is used for the
	// payment address throughout the tests.
	signKey crypto.PrivateKey
	//payAddr     *script.Address
	payAddr     cashaddr.Address
	payScript   []byte
	chainParams *model.BitcoinParams

	chain  *fakeChain
	txPool *mmempool.TxMempool

	keys map[string]*crypto.PrivateKey
}

// CreateCoinbaseTx returns a coinbase transaction with the requested number of
// outputs paying an appropriate subsidy based on the passed block height to the
// address associated with the harness.  It automatically uses a standard
// signature script that starts with the block height that is required by
// version 2 blocks.
func (p *poolHarness) CreateCoinbaseTx(blockHeight int32, numOutputs uint32) (*tx.Tx, error) {
	// Create standard coinbase script.
	extraNonce := int64(0)
	coinbaseScript := script.NewEmptyScript()
	coinbaseScript.PushInt64(int64(blockHeight))
	coinbaseScript.PushInt64(extraNonce)

	tx := tx.NewTx(0, tx.TxVersion)
	tx.AddTxIn(txin.NewTxIn(
		// Coinbase transactions have no inputs, so previous outpoint is
		// zero hash and max index.
		outpoint.NewOutPoint(util.Hash{}, math.MaxUint32),
		coinbaseScript,
		math.MaxUint32,
	))
	totalInput := model.GetBlockSubsidy(blockHeight, p.chainParams)
	amountPerOutput := int64(totalInput) / int64(numOutputs)
	remainder := int64(totalInput) - amountPerOutput*int64(numOutputs)
	for i := uint32(0); i < numOutputs; i++ {
		// Ensure the final output accounts for any remainder that might
		// be left from splitting the input amount.
		amount1 := amountPerOutput
		if i == numOutputs-1 {
			amount1 = amountPerOutput + remainder
		}
		tx.AddTxOut(txout.NewTxOut(
			amount.Amount(amount1),
			script.NewScriptRaw(p.payScript),
		))
	}

	return tx, nil
}

// CreateSignedTx creates a new signed transaction that consumes the provided
// inputs and generates the provided number of outputs by evenly splitting the
// total input amount.  All outputs will be to the payment script associated
// with the harness and all inputs are assumed to do the same.
func (p *poolHarness) CreateSignedTx(inputs []spendableOutput, numOutputs uint32) (*tx.Tx, error) {
	// Calculate the total input amount and split it amongst the requested
	// number of outputs.
	var totalInput amount.Amount
	for _, input := range inputs {
		totalInput += input.amount
	}
	amountPerOutput := int64(totalInput) / int64(numOutputs)
	remainder := int64(totalInput) - amountPerOutput*int64(numOutputs)

	tx := tx.NewTx(0, tx.TxVersion)
	for _, input := range inputs {
		tx.AddTxIn(txin.NewTxIn(
			&input.outPoint,
			nil,
			math.MaxUint32))
	}
	for i := uint32(0); i < numOutputs; i++ {
		// Ensure the final output accounts for any remainder that might
		// be left from splitting the input amount.
		amount1 := amountPerOutput
		if i == numOutputs-1 {
			amount1 = amountPerOutput + remainder
		}
		tx.AddTxOut(txout.NewTxOut(amount.Amount(amount1), script.NewScriptRaw(p.payScript)))
	}

	// Sign the new transaction.
	// for i := range tx.TxIn {
	// 	sigScript, err := txscript.SignatureScript(tx, i, p.payScript,
	// 		txscript.SigHashAll, p.signKey, true)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	tx.TxIn[i].SignatureScript = sigScript
	// }
	ltx.SignRawTransaction(tx, nil, p.keys, crypto.SigHashAll, p.chain.utxos)

	return tx, nil
}

// CreateTxChain creates a chain of zero-fee transactions (each subsequent
// transaction spends the entire amount from the previous one) with the first
// one spending the provided outpoint.  Each transaction spends the entire
// amount of the previous one and as such does not include any fees.
func (p *poolHarness) CreateTxChain(firstOutput spendableOutput, numTxns uint32) ([]*tx.Tx, error) {
	txChain := make([]*tx.Tx, 0, numTxns)
	prevOutPoint := &firstOutput.outPoint
	spendableAmount := firstOutput.amount
	for i := uint32(0); i < numTxns; i++ {
		// Create the transaction using the previous transaction output
		// and paying the full amount to the payment address associated
		// with the harness.
		tx := tx.NewTx(0, tx.TxVersion)
		tx.AddTxIn(txin.NewTxIn(
			prevOutPoint,
			nil,
			math.MaxUint32,
		))
		tx.AddTxOut(txout.NewTxOut(
			spendableAmount,
			script.NewScriptRaw(p.payScript),
		))

		// Sign the new transaction.
		// sigScript, err := txscript.SignatureScript(tx, 0, p.payScript,
		// 	txscript.SigHashAll, p.signKey, true)
		// if err != nil {
		// 	return nil, err
		// }
		// tx.TxIn[0].SignatureScript = sigScript
		err := ltx.SignRawTransaction(tx, nil, p.keys, crypto.SigHashAll|crypto.SigHashForkID, p.chain.utxos)
		if err != nil {
			panic(err)
		}
		txChain = append(txChain, tx)
		// fmt.Printf("prev:%v tx(%s)\n", tx.GetIns()[0].PreviousOutPoint, tx.GetHash())
		// Next transaction uses outputs from this one.
		prevOutPoint = &outpoint.OutPoint{Hash: tx.GetHash(), Index: 0}
		p.chain.utxos.AddCoin(prevOutPoint,
			utxo.NewCoin(tx.GetTxOut(0), 0, true),
			false)
	}
	// fmt.Printf("txchain: %v\n", txChain)
	return txChain, nil
}

func NewPrivateKey() crypto.PrivateKey {
	var keyBytes []byte
	for i := 0; i < 32; i++ {
		keyBytes = append(keyBytes, byte(rand.Uint32()%256))
	}
	return *crypto.PrivateKeyFromBytes(keyBytes)
}

func generateKeys(keyBytes []byte) (crypto.PrivateKey, crypto.PublicKey) {
	// var keyBytes []byte
	// for i := 0; i < 32; i++ {
	// 	keyBytes = append(keyBytes, byte(rand.Uint32()%256))
	// }
	privKey := *crypto.PrivateKeyFromBytes(keyBytes)
	return privKey, *privKey.PubKey()
}

// newPoolHarness returns a new instance of a pool harness initialized with a
// fake chain and a TxPool bound to it that is configured with a policy suitable
// for testing.  Also, the fake chain is populated with the returned spendable
// outputs so the caller can easily create new valid transactions which build
// off of it.
func newPoolHarness(chainParams *model.BitcoinParams) (*poolHarness, []spendableOutput, error) {
	// Use a hard coded key pair for deterministic results.
	keyBytes, err := hex.DecodeString("700868df1838811ffbdf918fb482c1f7e" +
		"ad62db4b97bd7012c23e726485e577d")
	if err != nil {
		return nil, nil, err
	}
	//signKey, signPub := btcec.PrivKeyFromBytes(btcec.S256(), keyBytes)
	signKey, signPub := generateKeys(keyBytes)

	// Generate associated pay-to-script-hash address and resulting payment
	// script.
	//pubKeyBytes := signPub.SerializeCompressed()
	//payPubKeyAddr, err := btcutil.NewAddressPubKey(pubKeyBytes, chainParams)
	var payAddr cashaddr.Address
	payAddr, err = cashaddr.NewCashAddressPubKeyHash(signPub.ToHash160(), chainParams)
	if err != nil {
		return nil, nil, err
	}
	// payAddr := payPubKeyAddr.AddressPubKeyHash()
	// pkScript, err := txscript.PayToAddrScript(payAddr)
	pkScript, err := cashaddr.CashPayToAddrScript(payAddr)
	if err != nil {
		return nil, nil, err
	}
	// Create a new fake chain and harness bound to it.
	chain := &fakeChain{utxos: utxo.NewEmptyCoinsMap()}
	harness := poolHarness{
		signKey:     signKey,
		payAddr:     payAddr,
		payScript:   pkScript,
		chainParams: chainParams,

		chain:  chain,
		txPool: mmempool.GetInstance(),
		keys:   make(map[string]*crypto.PrivateKey),
	}

	harness.keys[string(signPub.ToHash160())] = &signKey

	// Create a single coinbase transaction and add it to the harness
	// chain's utxo set and set the harness chain height such that the
	// coinbase will mature in the next block.  This ensures the txpool
	// accepts transactions which spend immature coinbases that will become
	// mature in the next block.
	numOutputs := uint32(1)
	outputs := make([]spendableOutput, 0, numOutputs)
	curHeight := harness.chain.BestHeight()
	coinbase, err := harness.CreateCoinbaseTx(curHeight+1, numOutputs)
	if err != nil {
		return nil, nil, err
	}
	//harness.chain.utxos.AddTxOuts(coinbase, curHeight+1)
	for outIdx := 0; outIdx < coinbase.GetOutsCount(); outIdx++ {
		harness.chain.utxos.AddCoin(outpoint.NewOutPoint(coinbase.GetHash(), uint32(outIdx)),
			utxo.NewCoin(coinbase.GetTxOut(outIdx), curHeight+1, true),
			false)
		//fmt.Printf("add %v to utxo\n", outpoint.NewOutPoint(coinbase.GetHash(), uint32(outIdx)))
	}
	for i := uint32(0); i < numOutputs; i++ {
		outputs = append(outputs, txOutToSpendableOut(coinbase, i))
	}
	harness.chain.SetHeight(int32(chainParams.CoinbaseMaturity) + curHeight)
	harness.chain.SetMedianTimePast(time.Now())

	utxo.GetUtxoCacheInstance().UpdateCoins(harness.chain.utxos, &util.Hash{})
	return &harness, outputs, nil
}

// testContext houses a test-related state that is useful to pass to helper
// functions as a single argument.
type testContext struct {
	t       *testing.T
	harness *poolHarness
}

// testPoolMembership tests the transaction pool associated with the provided
// test context to determine if the passed transaction matches the provided
// orphan pool and transaction pool status.  It also further determines if it
// should be reported as available by the HaveTransaction function based upon
// the two flags and tests that condition as well.
func testPoolMembership(tc *testContext, tx *tx.Tx, inOrphanPool, inTxPool bool) {
	gotOrphanPool := tc.harness.txPool.IsOrphanInPool(tx)
	if inOrphanPool != gotOrphanPool {
		_, file, line, _ := runtime.Caller(1)
		tc.t.Fatalf("%s:%d -- IsOrphanInPool: want %v, got %v", file,
			line, inOrphanPool, gotOrphanPool)
	}

	gotTxPool := tc.harness.txPool.IsTransactionInPool(tx)
	if inTxPool != gotTxPool {
		_, file, line, _ := runtime.Caller(1)
		tc.t.Fatalf("%s:%d -- IsTransactionInPool: want %v, got %v",
			file, line, inTxPool, gotTxPool)
	}
	gotHaveTx := tc.harness.txPool.HaveTransaction(tx)
	wantHaveTx := inOrphanPool || inTxPool
	if wantHaveTx != gotHaveTx {
		_, file, line, _ := runtime.Caller(1)
		tc.t.Fatalf("%s:%d -- HaveTransaction: want %v, got %v", file,
			line, wantHaveTx, gotHaveTx)
	}
}

// TestSimpleOrphanChain ensures that a simple chain of orphans is handled
// properly.  In particular, it generates a chain of single input, single output
// transactions and inserts them while skipping the first linking transaction so
// they are all orphans.  Finally, it adds the linking transaction and ensures
// the entire orphan chain is moved to the transaction pool.
func TestSimpleOrphanChain(t *testing.T) {
	// t.Parallel()
	os.RemoveAll("/tmp/dbtest")

	conf.Cfg = conf.InitConfig([]string{})
	// t.Parallel()
	uc := &utxo.UtxoConfig{Do: &db.DBOption{
		FilePath:  "/tmp/dbtest",
		CacheSize: 1 << 20,
	}}

	utxo.InitUtxoLruTip(uc)

	harness, spendableOuts, err := newPoolHarness(&model.MainNetParams)
	if err != nil {
		t.Fatalf("unable to create test pool: %v", err)
	}
	tc := &testContext{t, harness}

	// Create a chain of transactions rooted with the first spendable output
	// provided by the harness.
	//maxOrphans := uint32(harness.txPool.cfg.Policy.MaxOrphanTxs)
	maxOrphans := uint32(mmempool.DefaultMaxOrphanTransaction)
	chainedTxns, err := harness.CreateTxChain(spendableOuts[0], maxOrphans+1)
	if err != nil {
		t.Fatalf("unable to create transaction chain: %v", err)
	}

	// Ensure the orphans are accepted (only up to the maximum allowed so
	// none are evicted).
	for _, tx := range chainedTxns[1 : maxOrphans+1] {
		// acceptedTxns, err := harness.txPool.ProcessTransaction(tx, true,
		// 	false, 0)
		//acceptedTxns, _, err := service.ProcessTransaction(tx, 0)
		err := lmempool.AcceptTxToMemPool(tx, harness.chain.BestHeight(), false)
		service.HandleRejectedTx(tx, err, 0)
		if err == nil || !errcode.IsErrorCode(err, errcode.TxErrNoPreviousOut) {
			t.Fatalf("ProcessTransaction: failed to accept valid "+
				"orphan %v", err)
		}

		// Ensure the transaction is in the orphan pool, is not in the
		// transaction pool, and is reported as available.
		testPoolMembership(tc, tx, true, false)
	}

	// Add the transaction which completes the orphan chain and ensure they
	// all get accepted.  Notice the accept orphans flag is also false here
	// to ensure it has no bearing on whether or not already existing
	// orphans in the pool are linked.
	// acceptedTxns, err := harness.txPool.ProcessTransaction(chainedTxns[0],
	// 	false, false, 0)
	// acceptedTxns, _, err := service.ProcessTransaction(chainedTxns[0], 0)
	err = lmempool.AcceptTxToMemPool(chainedTxns[0], harness.chain.BestHeight(), false)
	if err != nil {
		t.Fatalf("ProcessTransaction: failed to accept valid "+
			"orphan %v", err)
	}
	acceptedTxns := lmempool.ProcessOrphan(chainedTxns[0], harness.chain.BestHeight(), false)
	if len(acceptedTxns) != len(chainedTxns)-1 {
		t.Fatalf("ProcessTransaction: reported accepted transactions "+
			"length does not match expected -- got %d, want %d",
			len(acceptedTxns), len(chainedTxns))
	}
	for _, tx := range acceptedTxns {
		// Ensure the transaction is no longer in the orphan pool, is
		// now in the transaction pool, and is reported as available.
		testPoolMembership(tc, tx, false, true)
	}
	os.RemoveAll("/tmp/dbtest")
}

// TestOrphanReject ensures that orphans are properly rejected when the allow
// orphans flag is not set on ProcessTransaction.
// func TestOrphanReject(t *testing.T) {
// 	// t.Parallel()
// 	os.RemoveAll("/tmp/dbtest")

// 	conf.Cfg = conf.InitConfig([]string{})
// 	// t.Parallel()
// 	uc := &utxo.UtxoConfig{Do: &db.DBOption{
// 		FilePath:  "/tmp/dbtest",
// 		CacheSize: 1 << 20,
// 	}}

// 	utxo.InitUtxoLruTip(uc)

// 	harness, outputs, err := newPoolHarness(&model.MainNetParams)
// 	if err != nil {
// 		t.Fatalf("unable to create test pool: %v", err)
// 	}
// 	tc := &testContext{t, harness}

// 	// Create a chain of transactions rooted with the first spendable output
// 	// provided by the harness.
// 	//maxOrphans := uint32(harness.txPool.cfg.Policy.MaxOrphanTxs)
// 	maxOrphans := uint32(mmempool.DefaultMaxOrphanTransaction)
// 	chainedTxns, err := harness.CreateTxChain(outputs[0], maxOrphans+1)
// 	if err != nil {
// 		t.Fatalf("unable to create transaction chain: %v", err)
// 	}

// 	// Ensure orphans are rejected when the allow orphans flag is not set.
// 	for _, tx := range chainedTxns[1:] {
// 		// acceptedTxns, err := harness.txPool.ProcessTransaction(tx, false,
// 		// 	false, 0)
// 		//acceptedTxns, _, err := service.ProcessTransaction(tx, 0)
// 		err := lmempool.AcceptTxToMemPool(tx, harness.chain.BestHeight(), false)
// 		service.HandleRejectedTx(tx, err, 0)
// 		if err == nil || !errcode.IsErrorCode(err, errcode.TxErrNoPreviousOut) {
// 			t.Fatalf("ProcessTransaction: did not fail on orphan "+
// 				"%v when allow orphans flag is false", tx.GetHash())
// 		}
// 		// TODO: add it back
// 		// expectedErr := RuleError{}
// 		// if reflect.TypeOf(err) != reflect.TypeOf(expectedErr) {
// 		// 	t.Fatalf("ProcessTransaction: wrong error got: <%T> %v, "+
// 		// 		"want: <%T>", err, err, expectedErr)
// 		// }
// 		// code, extracted := extractRejectCode(err)
// 		// if !extracted {
// 		// 	t.Fatalf("ProcessTransaction: failed to extract reject "+
// 		// 		"code from error %q", err)
// 		// }
// 		// if code != wire.RejectDuplicate {
// 		// 	t.Fatalf("ProcessTransaction: unexpected reject code "+
// 		// 		"-- got %v, want %v", code, wire.RejectDuplicate)
// 		// }

// 		// Ensure no transactions were reported as accepted.
// 		// if len(acceptedTxns) != 0 {
// 		// 	t.Fatal("ProcessTransaction: reported %d accepted "+
// 		// 		"transactions from failed orphan attempt",
// 		// 		len(acceptedTxns))
// 		// }

// 		testPoolMembership(tc, tx, true, false)
// 	}
// 	os.RemoveAll("/tmp/dbtest")
// }

// TestOrphanEviction ensures that exceeding the maximum number of orphans
// evicts entries to make room for the new ones.
// FIXME: since implementation of eviction is different from btcd. this test is not
// suitable for copernicus. We may add it back when we improve our implementation.
// func TestOrphanEviction(t *testing.T) {
// 	//t.Parallel()
// 	conf.Cfg = conf.InitConfig([]string{})
// 	// t.Parallel()
// 	uc := &utxo.UtxoConfig{Do: &db.DBOption{
// 		FilePath:  "/tmp/dbtest",
// 		CacheSize: 1 << 20,
// 	}}

// 	utxo.InitUtxoLruTip(uc)

// 	harness, outputs, err := newPoolHarness(&model.MainNetParams)
// 	if err != nil {
// 		t.Fatalf("unable to create test pool: %v", err)
// 	}
// 	tc := &testContext{t, harness}

// 	// Create a chain of transactions rooted with the first spendable output
// 	// provided by the harness that is long enough to be able to force
// 	// several orphan evictions.
// 	//maxOrphans := uint32(harness.txPool.cfg.Policy.MaxOrphanTxs)
// 	maxOrphans := uint32(mmempool.DefaultMaxOrphanTransaction)
// 	chainedTxns, err := harness.CreateTxChain(outputs[0], maxOrphans+5)
// 	if err != nil {
// 		t.Fatalf("unable to create transaction chain: %v", err)
// 	}

// 	// Add enough orphans to exceed the max allowed while ensuring they are
// 	// all accepted.  This will cause an eviction.
// 	for i, tx := range chainedTxns[1:] {
// 		// acceptedTxns, err := harness.txPool.ProcessTransaction(tx, true,
// 		// 	false, 0)
// 		// acceptedTxns, _, err := service.ProcessTransaction(tx, 0)
// 		err := lmempool.AcceptTxToMemPool(tx, harness.chain.BestHeight(), false)
// 		if !errcode.IsErrorCode(err, errcode.TxErrNoPreviousOut) {
// 			t.Fatalf("ProcessTransaction: failed to accept valid "+
// 				"orphan %v", err)
// 		}
// 		service.HandleRejectedTx(tx, err, 0)

// 		fmt.Printf("i=%d tx(%s))\n", i, tx.GetHash())
// 		// Ensure the transaction is in the orphan pool, is not in the
// 		// transaction pool, and is reported as available.
// 		testPoolMembership(tc, tx, true, false)
// 	}

// 	// Figure out which transactions were evicted and make sure the number
// 	// evicted matches the expected number.
// 	var evictedTxns []*tx.Tx
// 	for _, tx := range chainedTxns[1:] {
// 		if !harness.txPool.IsOrphanInPool(tx) {
// 			evictedTxns = append(evictedTxns, tx)
// 		}
// 	}
// 	expectedEvictions := len(chainedTxns) - 1 - int(maxOrphans)
// 	if len(evictedTxns) != expectedEvictions {
// 		t.Fatalf("unexpected number of evictions -- got %d, want %d",
// 			len(evictedTxns), expectedEvictions)
// 	}

// 	// Ensure none of the evicted transactions ended up in the transaction
// 	// pool.
// 	for _, tx := range evictedTxns {
// 		testPoolMembership(tc, tx, false, false)
// 	}
// 	os.RemoveAll("/tmp/dbtest")
// }

// TestBasicOrphanRemoval ensure that orphan removal works as expected when an
// orphan that doesn't exist is removed  both when there is another orphan that
// redeems it and when there is not.
// func TestBasicOrphanRemoval(t *testing.T) {
// 	t.Parallel()

// 	const maxOrphans = 4
// 	harness, spendableOuts, err := newPoolHarness(&model.MainNetParams)
// 	if err != nil {
// 		t.Fatalf("unable to create test pool: %v", err)
// 	}
// 	harness.txPool.cfg.Policy.MaxOrphanTxs = maxOrphans
// 	tc := &testContext{t, harness}

// 	// Create a chain of transactions rooted with the first spendable output
// 	// provided by the harness.
// 	chainedTxns, err := harness.CreateTxChain(spendableOuts[0], maxOrphans+1)
// 	if err != nil {
// 		t.Fatalf("unable to create transaction chain: %v", err)
// 	}

// 	// Ensure the orphans are accepted (only up to the maximum allowed so
// 	// none are evicted).
// 	for _, tx := range chainedTxns[1 : maxOrphans+1] {
// 		acceptedTxns, err := harness.txPool.ProcessTransaction(tx, true,
// 			false, 0)
// 		if err != nil {
// 			t.Fatalf("ProcessTransaction: failed to accept valid "+
// 				"orphan %v", err)
// 		}

// 		// Ensure no transactions were reported as accepted.
// 		if len(acceptedTxns) != 0 {
// 			t.Fatalf("ProcessTransaction: reported %d accepted "+
// 				"transactions from what should be an orphan",
// 				len(acceptedTxns))
// 		}

// 		// Ensure the transaction is in the orphan pool, not in the
// 		// transaction pool, and reported as available.
// 		testPoolMembership(tc, tx, true, false)
// 	}

// 	// Attempt to remove an orphan that has no redeemers and is not present,
// 	// and ensure the state of all other orphans are unaffected.
// 	nonChainedOrphanTx, err := harness.CreateSignedTx([]spendableOutput{{
// 		amount:   btcutil.Amount(5000000000),
// 		outPoint: wire.OutPoint{Hash: chainhash.Hash{}, Index: 0},
// 	}}, 1)
// 	if err != nil {
// 		t.Fatalf("unable to create signed tx: %v", err)
// 	}

// 	harness.txPool.RemoveOrphan(nonChainedOrphanTx)
// 	testPoolMembership(tc, nonChainedOrphanTx, false, false)
// 	for _, tx := range chainedTxns[1 : maxOrphans+1] {
// 		testPoolMembership(tc, tx, true, false)
// 	}

// 	// Attempt to remove an orphan that has a existing redeemer but itself
// 	// is not present and ensure the state of all other orphans (including
// 	// the one that redeems it) are unaffected.
// 	harness.txPool.RemoveOrphan(chainedTxns[0])
// 	testPoolMembership(tc, chainedTxns[0], false, false)
// 	for _, tx := range chainedTxns[1 : maxOrphans+1] {
// 		testPoolMembership(tc, tx, true, false)
// 	}

// 	// Remove each orphan one-by-one and ensure they are removed as
// 	// expected.
// 	for _, tx := range chainedTxns[1 : maxOrphans+1] {
// 		harness.txPool.RemoveOrphan(tx)
// 		testPoolMembership(tc, tx, false, false)
// 	}
// }

// TestOrphanChainRemoval ensure that orphan chains (orphans that spend outputs
// from other orphans) are removed as expected.
// func TestOrphanChainRemoval(t *testing.T) {
// 	t.Parallel()

// 	const maxOrphans = 10
// 	harness, spendableOuts, err := newPoolHarness(&chaincfg.MainNetParams)
// 	if err != nil {
// 		t.Fatalf("unable to create test pool: %v", err)
// 	}
// 	harness.txPool.cfg.Policy.MaxOrphanTxs = maxOrphans
// 	tc := &testContext{t, harness}

// 	// Create a chain of transactions rooted with the first spendable output
// 	// provided by the harness.
// 	chainedTxns, err := harness.CreateTxChain(spendableOuts[0], maxOrphans+1)
// 	if err != nil {
// 		t.Fatalf("unable to create transaction chain: %v", err)
// 	}

// 	// Ensure the orphans are accepted (only up to the maximum allowed so
// 	// none are evicted).
// 	for _, tx := range chainedTxns[1 : maxOrphans+1] {
// 		acceptedTxns, err := harness.txPool.ProcessTransaction(tx, true,
// 			false, 0)
// 		if err != nil {
// 			t.Fatalf("ProcessTransaction: failed to accept valid "+
// 				"orphan %v", err)
// 		}

// 		// Ensure no transactions were reported as accepted.
// 		if len(acceptedTxns) != 0 {
// 			t.Fatalf("ProcessTransaction: reported %d accepted "+
// 				"transactions from what should be an orphan",
// 				len(acceptedTxns))
// 		}

// 		// Ensure the transaction is in the orphan pool, not in the
// 		// transaction pool, and reported as available.
// 		testPoolMembership(tc, tx, true, false)
// 	}

// 	// Remove the first orphan that starts the orphan chain without the
// 	// remove redeemer flag set and ensure that only the first orphan was
// 	// removed.
// 	harness.txPool.mtx.Lock()
// 	harness.txPool.removeOrphan(chainedTxns[1], false)
// 	harness.txPool.mtx.Unlock()
// 	testPoolMembership(tc, chainedTxns[1], false, false)
// 	for _, tx := range chainedTxns[2 : maxOrphans+1] {
// 		testPoolMembership(tc, tx, true, false)
// 	}

// 	// Remove the first remaining orphan that starts the orphan chain with
// 	// the remove redeemer flag set and ensure they are all removed.
// 	harness.txPool.mtx.Lock()
// 	harness.txPool.removeOrphan(chainedTxns[2], true)
// 	harness.txPool.mtx.Unlock()
// 	for _, tx := range chainedTxns[2 : maxOrphans+1] {
// 		testPoolMembership(tc, tx, false, false)
// 	}
// }

// TestMultiInputOrphanDoubleSpend ensures that orphans that spend from an
// output that is spend by another transaction entering the pool are removed.
// func TestMultiInputOrphanDoubleSpend(t *testing.T) {
// 	t.Parallel()

// 	const maxOrphans = 4
// 	harness, outputs, err := newPoolHarness(&chaincfg.MainNetParams)
// 	if err != nil {
// 		t.Fatalf("unable to create test pool: %v", err)
// 	}
// 	harness.txPool.cfg.Policy.MaxOrphanTxs = maxOrphans
// 	tc := &testContext{t, harness}

// 	// Create a chain of transactions rooted with the first spendable output
// 	// provided by the harness.
// 	chainedTxns, err := harness.CreateTxChain(outputs[0], maxOrphans+1)
// 	if err != nil {
// 		t.Fatalf("unable to create transaction chain: %v", err)
// 	}

// 	// Start by adding the orphan transactions from the generated chain
// 	// except the final one.
// 	for _, tx := range chainedTxns[1:maxOrphans] {
// 		acceptedTxns, err := harness.txPool.ProcessTransaction(tx, true,
// 			false, 0)
// 		if err != nil {
// 			t.Fatalf("ProcessTransaction: failed to accept valid "+
// 				"orphan %v", err)
// 		}
// 		if len(acceptedTxns) != 0 {
// 			t.Fatalf("ProcessTransaction: reported %d accepted transactions "+
// 				"from what should be an orphan", len(acceptedTxns))
// 		}
// 		testPoolMembership(tc, tx, true, false)
// 	}

// 	// Ensure a transaction that contains a double spend of the same output
// 	// as the second orphan that was just added as well as a valid spend
// 	// from that last orphan in the chain generated above (and is not in the
// 	// orphan pool) is accepted to the orphan pool.  This must be allowed
// 	// since it would otherwise be possible for a malicious actor to disrupt
// 	// tx chains.
// 	doubleSpendTx, err := harness.CreateSignedTx([]spendableOutput{
// 		txOutToSpendableOut(chainedTxns[1], 0),
// 		txOutToSpendableOut(chainedTxns[maxOrphans], 0),
// 	}, 1)
// 	if err != nil {
// 		t.Fatalf("unable to create signed tx: %v", err)
// 	}
// 	acceptedTxns, err := harness.txPool.ProcessTransaction(doubleSpendTx,
// 		true, false, 0)
// 	if err != nil {
// 		t.Fatalf("ProcessTransaction: failed to accept valid orphan %v",
// 			err)
// 	}
// 	if len(acceptedTxns) != 0 {
// 		t.Fatalf("ProcessTransaction: reported %d accepted transactions "+
// 			"from what should be an orphan", len(acceptedTxns))
// 	}
// 	testPoolMembership(tc, doubleSpendTx, true, false)

// 	// Add the transaction which completes the orphan chain and ensure the
// 	// chain gets accepted.  Notice the accept orphans flag is also false
// 	// here to ensure it has no bearing on whether or not already existing
// 	// orphans in the pool are linked.
// 	//
// 	// This will cause the shared output to become a concrete spend which
// 	// will in turn must cause the double spending orphan to be removed.
// 	acceptedTxns, err = harness.txPool.ProcessTransaction(chainedTxns[0],
// 		false, false, 0)
// 	if err != nil {
// 		t.Fatalf("ProcessTransaction: failed to accept valid tx %v", err)
// 	}
// 	if len(acceptedTxns) != maxOrphans {
// 		t.Fatalf("ProcessTransaction: reported accepted transactions "+
// 			"length does not match expected -- got %d, want %d",
// 			len(acceptedTxns), maxOrphans)
// 	}
// 	for _, txD := range acceptedTxns {
// 		// Ensure the transaction is no longer in the orphan pool, is
// 		// in the transaction pool, and is reported as available.
// 		testPoolMembership(tc, txD.Tx, false, true)
// 	}

// 	// Ensure the double spending orphan is no longer in the orphan pool and
// 	// was not moved to the transaction pool.
// 	testPoolMembership(tc, doubleSpendTx, false, false)
// }

// TestCheckSpend tests that CheckSpend returns the expected spends found in
// the mempool.
// func TestCheckSpend(t *testing.T) {
// 	os.RemoveAll("/tmp/dbtest")
// 	conf.Cfg = conf.InitConfig([]string{})
// 	// t.Parallel()
// 	uc := &utxo.UtxoConfig{Do: &db.DBOption{
// 		FilePath:  "/tmp/dbtest",
// 		CacheSize: 1 << 20,
// 	}}

// 	utxo.InitUtxoLruTip(uc)

// 	harness, outputs, err := newPoolHarness(&model.MainNetParams)
// 	if err != nil {
// 		t.Fatalf("unable to create test pool: %v", err)
// 	}
// 	// The mempool is empty, so none of the spendable outputs should have a
// 	// spend there.
// 	for _, op := range outputs {
// 		//spend := harness.txPool.CheckSpend(op.outPoint)
// 		spend := harness.txPool.HasSpentOut(&op.outPoint)
// 		if spend != nil {
// 			t.Fatalf("Unexpeced spend found in pool: %v", spend)
// 		}
// 	}

// 	// Create a chain of transactions rooted with the first spendable
// 	// output provided by the harness.
// 	const txChainLength = 5
// 	chainedTxns, err := harness.CreateTxChain(outputs[0], txChainLength)
// 	if err != nil {
// 		t.Fatalf("unable to create transaction chain: %v", err)
// 	}
// 	for _, tx := range chainedTxns {
// 		// _, err := harness.txPool.ProcessTransaction(tx, true,
// 		// 	false, 0)
// 		//fmt.Printf("process %v tx(%s)\n", tx.GetIns()[0].PreviousOutPoint, tx.GetHash())
// 		//_, _, err := service.ProcessTransaction(tx, 0)
// 		err := lmempool.AcceptTxToMemPool(tx, harness.chain.BestHeight(), false)
// 		if err != nil {
// 			t.Fatalf("ProcessTransaction: failed to accept "+
// 				"tx(%s): %v", tx.GetHash(), err)
// 		}
// 	}

// 	// The first tx in the chain should be the spend of the spendable
// 	// output.
// 	op := outputs[0].outPoint
// 	spend := harness.txPool.HasSpentOut(&op)
// 	if spend.Tx != chainedTxns[0] {
// 		t.Fatalf("expected %v to be spent by %v, instead "+
// 			"got %v", op, chainedTxns[0], spend)
// 	}

// 	// Now all but the last tx should be spent by the next.
// 	for i := 0; i < len(chainedTxns)-1; i++ {
// 		op = outpoint.OutPoint{
// 			Hash:  chainedTxns[i].GetHash(),
// 			Index: 0,
// 		}
// 		expSpend := chainedTxns[i+1]
// 		spend = harness.txPool.HasSpentOut(&op)
// 		if spend.Tx != expSpend {
// 			t.Fatalf("expected %v to be spent by %v, instead "+
// 				"got %v", op, expSpend, spend)
// 		}
// 	}

// 	// The last tx should have no spend.
// 	op = outpoint.OutPoint{
// 		Hash:  chainedTxns[txChainLength-1].GetHash(),
// 		Index: 0,
// 	}
// 	spend = harness.txPool.HasSpentOut(&op)
// 	if spend != nil {
// 		t.Fatalf("Unexpeced spend found in pool: %v", spend)
// 	}
// 	os.RemoveAll("/tmp/dbtest")
// }
