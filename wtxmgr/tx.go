/*
 * Copyright (c) 2013-2015 The btcsuite developers
 * Copyright (c) 2015 The Decred developers
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package wtxmgr

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/golangcrypto/ripemd160"
	"github.com/decred/dcrd/blockchain"
	"github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/txscript"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrutil"
	"github.com/decred/dcrwallet/walletdb"
)

// BehaviorFlags is a bitmask defining tweaks to the normal behavior when
// performing chain processing and consensus rules checks.
type BehaviorFlags uint32

const (
	OP_NONSTAKE = txscript.OP_NOP10

	BFBalanceAll BehaviorFlags = iota
	BFBalanceLockedStake
	BFBalanceSpendable
	BFBalanceFullScan
)

// Block contains the minimum amount of data to uniquely identify any block on
// either the best or side chain.
type Block struct {
	Hash   chainhash.Hash
	Height int32
}

// BlockMeta contains the unique identification for a block and any metadata
// pertaining to the block.  At the moment, this additional metadata only
// includes the block time from the block header.
type BlockMeta struct {
	Block
	Time     time.Time
	VoteBits uint16
}

// blockRecord is an in-memory representation of the block record saved in the
// database.
type blockRecord struct {
	Block
	Time         time.Time
	VoteBits     uint16
	transactions []chainhash.Hash
}

// incidence records the block hash and blockchain height of a mined transaction.
// Since a transaction hash alone is not enough to uniquely identify a mined
// transaction (duplicate transaction hashes are allowed), the incidence is used
// instead.
type incidence struct {
	txHash chainhash.Hash
	block  Block
}

// indexedIncidence records the transaction incidence and an input or output
// index.
type indexedIncidence struct {
	incidence
	index uint32
}

// debit records the debits a transaction record makes from previous wallet
// transaction credits.
type debit struct {
	txHash chainhash.Hash
	index  uint32
	amount dcrutil.Amount
	spends indexedIncidence
}

// credit describes a transaction output which was or is spendable by wallet.
type credit struct {
	outPoint   wire.OutPoint
	block      Block
	amount     dcrutil.Amount
	change     bool
	spentBy    indexedIncidence // Index == ^uint32(0) if unspent
	opCode     uint8
	isCoinbase bool
}

// TxRecord represents a transaction managed by the Store.
type TxRecord struct {
	MsgTx        wire.MsgTx
	Hash         chainhash.Hash
	Received     time.Time
	SerializedTx []byte // Optional: may be nil
	TxType       stake.TxType
}

// NewTxRecord creates a new transaction record that may be inserted into the
// store.  It uses memoization to save the transaction hash and the serialized
// transaction.
func NewTxRecord(serializedTx []byte, received time.Time) (*TxRecord, error) {
	rec := &TxRecord{
		Received:     received,
		SerializedTx: serializedTx,
	}
	err := rec.MsgTx.Deserialize(bytes.NewReader(serializedTx))
	if err != nil {
		str := "failed to deserialize transaction"
		return nil, storeError(ErrInput, str, err)
	}
	rec.TxType = stake.DetermineTxType(dcrutil.NewTx(&rec.MsgTx))
	hash := rec.MsgTx.TxSha()
	copy(rec.Hash[:], hash[:])
	return rec, nil
}

// NewTxRecordFromMsgTx creates a new transaction record that may be inserted
// into the store.
func NewTxRecordFromMsgTx(msgTx *wire.MsgTx, received time.Time) (*TxRecord,
	error) {
	buf := bytes.NewBuffer(make([]byte, 0, msgTx.SerializeSize()))
	err := msgTx.Serialize(buf)
	if err != nil {
		str := "failed to serialize transaction"
		return nil, storeError(ErrInput, str, err)
	}
	rec := &TxRecord{
		MsgTx:        *msgTx,
		Received:     received,
		SerializedTx: buf.Bytes(),
	}
	rec.TxType = stake.DetermineTxType(dcrutil.NewTx(&rec.MsgTx))
	hash := rec.MsgTx.TxSha()
	copy(rec.Hash[:], hash[:])
	return rec, nil
}

// MultisigOut represents a spendable multisignature outpoint contain
// a script hash.
type MultisigOut struct {
	OutPoint     *wire.OutPoint
	Tree         int8
	ScriptHash   [ripemd160.Size]byte
	M            uint8
	N            uint8
	TxHash       chainhash.Hash
	BlockHash    chainhash.Hash
	BlockHeight  uint32
	Amount       dcrutil.Amount
	Spent        bool
	SpentBy      chainhash.Hash
	SpentByIndex uint32
}

// Credit is the type representing a transaction output which was spent or
// is still spendable by wallet.  A UTXO is an unspent Credit, but not all
// Credits are UTXOs.
type Credit struct {
	wire.OutPoint
	BlockMeta
	Amount       dcrutil.Amount
	PkScript     []byte
	Received     time.Time
	FromCoinBase bool
}

// Store implements a transaction store for storing and managing wallet
// transactions.
type Store struct {
	mutex    *sync.Mutex
	isClosed bool

	namespace   walletdb.Namespace
	chainParams *chaincfg.Params
}

// SortedTxRecords is a list of transaction records that can be sorted.
type SortableTxRecords []*TxRecord

func (p SortableTxRecords) Len() int { return len(p) }
func (p SortableTxRecords) Less(i, j int) bool {
	return p[i].Received.Before(p[j].Received)
}
func (p SortableTxRecords) Swap(i, j int) { p[i], p[j] = p[j], p[i] }

// pruneOldTickets prunes old stake tickets from before ticketCutoff from the
// database. Maybe corrupt the database if the user Ctrl+C's during this short
// function.
func (s *Store) pruneOldTickets(ns walletdb.Bucket,
	ticketCutoff time.Duration) error {
	current := time.Now()
	log.Infof("Pruning old tickets from before from the transaction " +
		"database, please do not attempt to close your wallet.")

	minedBalance, err := fetchMinedBalance(ns)
	if err != nil {
		return err
	}

	// Cache for records, so we can sort them.
	var savedSStxs SortableTxRecords

	err = ns.Bucket(bucketUnmined).ForEach(func(k, v []byte) error {
		// TODO: Parsing transactions from the db may be a little
		// expensive.  It's possible the caller only wants the
		// serialized transactions.
		var txHash chainhash.Hash
		err := readRawUnminedHash(k, &txHash)
		if err != nil {
			return err
		}

		var rec TxRecord
		err = readRawTxRecord(&txHash, v, &rec)
		if err != nil {
			return err
		}

		txType := stake.DetermineTxType(dcrutil.NewTx(&rec.MsgTx))

		// Prune all old tickets.
		if current.Sub(rec.Received) > ticketCutoff &&
			(txType == stake.TxTypeSStx) {
			savedSStxs = append(savedSStxs, &rec)
		}

		return err
	})
	if err != nil {
		return err
	}

	// The transactions need to be sorted by date inserted in
	// case one SStx spends from the change of another, in
	// which case ordering matters.
	sort.Sort(sort.Reverse(savedSStxs))
	for _, rec := range savedSStxs {
		// Return all the inputs to their unspent state.
		for _, txi := range rec.MsgTx.TxIn {
			// Figure out where the used outpoint was
			// from, either a credit or an unmined credit.

			// Make sure the input exists.
			prevOut := &txi.PreviousOutPoint
			prevOutKey := canonicalOutPoint(&prevOut.Hash,
				prevOut.Index)
			valUMInput := existsRawUnminedInput(ns, prevOutKey)
			if valUMInput == nil {
				return fmt.Errorf("missing unmined input")
			}

			var keyCredit []byte
			var valCredit []byte
			// Look up and see if it spends a mined credit.
			spendsMinedCredit := true
			err := ns.Bucket(bucketCredits).ForEach(
				func(lkc, lvc []byte) error {
					lcHash := extractRawCreditTxHash(lkc)
					lcIndex := extractRawCreditIndex(lkc)

					lcOp := canonicalOutPoint(&lcHash,
						lcIndex)
					if bytes.Equal(lcOp, prevOutKey) {
						keyCredit = lkc
						valCredit = lvc
					}

					return nil
				})
			if err != nil {
				return err
			}

			// Should spend an unmined output, then.
			// Find it.
			if valCredit == nil || keyCredit == nil {
				valRawUMC := existsRawUnminedCredit(ns, prevOutKey)
				if valRawUMC != nil {
					spendsMinedCredit = false
					keyCredit = prevOutKey
					valCredit = valRawUMC
				}
			}

			if valCredit == nil || keyCredit == nil {
				return fmt.Errorf("credit missing")
			}

			// Unspending mined credits increments our balance,
			// so there are a lot of extra steps we have to take
			// to patch that.
			if spendsMinedCredit {
				var amt dcrutil.Amount
				amt, err = unspendRawCredit(ns, keyCredit)
				if err != nil {
					return err
				}

				// If the credit was previously removed by being
				// double spent, the credit amount is zero.  Only
				// mark the previously spent credit as unspent
				// if it still exists.
				if amt == 0 {
					continue
				}
				unspentVal, err := fetchRawCreditUnspentValue(keyCredit)
				if err != nil {
					return err
				}
				minedBalance = minedBalance + amt
				err = putRawUnspent(ns, prevOutKey, unspentVal)
				if err != nil {
					return err
				}
			} else {
				// An unmined output was used as an input, mark it
				// unspent. However, tx mananger doesn't currently
				// keep spending data for unspent outputs so do
				// nothing??? cj
			}

			// Delete the unmined input.
			err = deleteRawUnminedInput(ns, prevOutKey)
			if err != nil {
				return err
			}
		}

		for idx, txo := range rec.MsgTx.TxOut {
			class := txscript.GetScriptClass(txo.Version, txo.PkScript)

			// For tickets, the only thing we care about are submission
			// and change tagged outputs.
			switch {
			case class == txscript.StakeSubmissionTy ||
				class == txscript.StakeSubChangeTy:
				// See if we inserted the unmined credit. If so,
				// remove it.
				kOut := canonicalOutPoint(&rec.Hash, uint32(idx))
				val := existsRawUnminedCredit(ns, kOut)
				if val != nil {
					err = deleteRawUnminedCredit(ns, kOut)
					if err != nil {
						return err
					}
				}
			}
		}

		// Delete the transaction record itself.
		err = deleteRawUnmined(ns, rec.Hash[:])
		if err != nil {
			return err
		}
	}

	// Update our balance.
	return putMinedBalance(ns, minedBalance)
}

// Open opens the wallet transaction store from a walletdb namespace.  If the
// store does not exist, ErrNoExist is returned.  Existing stores will be
// upgraded to new database formats as necessary.
func Open(namespace walletdb.Namespace, pruneTickets bool,
	chainParams *chaincfg.Params) (*Store, error) {
	// Open the store, upgrading to the latest version as needed.
	err := openStore(namespace)
	if err != nil {
		return nil, err
	}

	s := &Store{new(sync.Mutex), false, namespace, chainParams}

	// Skip pruning on simnet, because the adjustment times are
	// so short.
	if pruneTickets && chainParams.Name != "simnet" {
		ticketCutoff := chainParams.TimePerBlock *
			time.Duration(chainParams.WorkDiffWindowSize)
		err = scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
			var err error
			err = s.pruneOldTickets(ns, ticketCutoff)
			return err
		})
		if err != nil {
			return nil, err
		}
	}

	return s, nil
}

// Create creates and opens a new persistent transaction store in the walletdb
// namespace.  Creating the store when one already exists in this namespace will
// error with ErrAlreadyExists.
func Create(namespace walletdb.Namespace, chainParams *chaincfg.Params) (*Store,
	error) {
	err := createStore(namespace)
	if err != nil {
		return nil, err
	}
	return &Store{new(sync.Mutex), false, namespace, chainParams}, nil
}

// Close safely closes the transaction manager by waiting for the mutex to
// unlock and then preventing any new calls to the transaction manager.
func (s *Store) Close() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.isClosed = true
}

// InsertBlock inserts a block into the block database if it doesn't already
// exist.
func (s *Store) InsertBlock(bm *BlockMeta) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	return scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		return s.insertBlock(ns, bm)
	})
}

func (s *Store) insertBlock(ns walletdb.Bucket, bm *BlockMeta) error {
	// Check to see if the block already exists; nothing to add in this case.
	blockKey, blockVal := existsBlockRecord(ns, bm.Height)
	if blockVal != nil {
		return nil
	}

	blockVal = valueBlockRecordEmpty(bm)

	return putRawBlockRecord(ns, blockKey, blockVal)
}

// GetBlockHash fetches the block hash for the block at the given height,
// and returns an error if it's missing.
func (s *Store) GetBlockHash(height int32) (chainhash.Hash, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return chainhash.Hash{}, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var blockHash chainhash.Hash
	var err error
	err = scopedView(s.namespace, func(ns walletdb.Bucket) error {
		blockHash, err = s.getBlockHash(ns, height)
		return err
	})

	return blockHash, err
}

func (s *Store) getBlockHash(ns walletdb.Bucket, height int32) (chainhash.Hash,
	error) {
	br, err := fetchBlockRecord(ns, height)
	if err != nil {
		return chainhash.Hash{}, err
	}

	return br.Block.Hash, nil
}

// moveMinedTx moves a transaction record from the unmined buckets to block
// buckets.
func (s *Store) moveMinedTx(ns walletdb.Bucket, rec *TxRecord, recKey,
	recVal []byte, block *BlockMeta) error {
	log.Debugf("Marking unconfirmed transaction %v mined in block %d",
		&rec.Hash, block.Height)

	// Insert block record as needed.
	blockKey, blockVal := existsBlockRecord(ns, block.Height)
	var err error
	if blockVal == nil {
		blockVal = valueBlockRecord(block, &rec.Hash)
	} else {
		blockVal, err = appendRawBlockRecord(blockVal, &rec.Hash)
		if err != nil {
			return err
		}
	}
	err = putRawBlockRecord(ns, blockKey, blockVal)
	if err != nil {
		return err
	}

	err = putRawTxRecord(ns, recKey, recVal)
	if err != nil {
		return err
	}
	minedBalance, err := fetchMinedBalance(ns)
	if err != nil {
		return err
	}

	// For all mined transactions with unspent credits spent by this
	// transaction, mark each spent, remove from the unspents map, and
	// insert a debit record for the spent credit.
	debitIncidence := indexedIncidence{
		incidence: incidence{txHash: rec.Hash, block: block.Block},
		// index set for each rec input below.
	}
	for i, input := range rec.MsgTx.TxIn {
		unspentKey, credKey := existsUnspent(ns, &input.PreviousOutPoint)
		if credKey == nil {
			continue
		}
		debitIncidence.index = uint32(i)
		amt, err := spendCredit(ns, credKey, &debitIncidence)
		if err != nil {
			return err
		}

		credVal := existsRawCredit(ns, credKey)
		if credVal == nil {
			return fmt.Errorf("missing credit value")
		}
		creditOpCode := fetchRawCreditTagOpCode(credVal)

		// Do not decrement ticket amounts.
		if !(creditOpCode == txscript.OP_SSTX) {
			minedBalance -= amt
		}
		err = deleteRawUnspent(ns, unspentKey)
		if err != nil {
			return err
		}

		err = putDebit(ns, &rec.Hash, uint32(i), amt, &block.Block, credKey)
		if err != nil {
			return err
		}

		err = deleteRawUnminedInput(ns, unspentKey)
		if err != nil {
			return err
		}
	}

	// For each output of the record that is marked as a credit, if the
	// output is marked as a credit by the unconfirmed store, remove the
	// marker and mark the output as a credit in the db.
	//
	// Moved credits are added as unspents, even if there is another
	// unconfirmed transaction which spends them.
	cred := credit{
		outPoint: wire.OutPoint{Hash: rec.Hash},
		block:    block.Block,
		spentBy:  indexedIncidence{index: ^uint32(0)},
	}
	it := makeUnminedCreditIterator(ns, &rec.Hash)
	for it.next() {
		// TODO: This should use the raw apis.  The credit value (it.cv)
		// can be moved from unmined directly to the credits bucket.
		// The key needs a modification to include the block
		// height/hash.
		index, err := fetchRawUnminedCreditIndex(it.ck)
		if err != nil {
			return err
		}
		amount, change, err := fetchRawUnminedCreditAmountChange(it.cv)
		if err != nil {
			return err
		}
		cred.outPoint.Index = index
		cred.amount = amount
		cred.change = change
		cred.opCode = fetchRawUnminedCreditTagOpcode(it.cv)
		cred.isCoinbase = fetchRawUnminedCreditTagIsCoinbase(it.cv)

		err = it.delete()
		if err != nil {
			return err
		}
		err = putUnspentCredit(ns, &cred)
		if err != nil {
			return err
		}
		err = putUnspent(ns, &cred.outPoint, &block.Block)
		if err != nil {
			return err
		}

		// Do not increment ticket credits.
		if !(cred.opCode == txscript.OP_SSTX) {
			minedBalance += amount
		}
	}
	if it.err != nil {
		return it.err
	}

	err = putMinedBalance(ns, minedBalance)
	if err != nil {
		return err
	}

	return deleteRawUnmined(ns, rec.Hash[:])
}

// InsertTx records a transaction as belonging to a wallet's transaction
// history.  If block is nil, the transaction is considered unspent, and the
// transaction's index must be unset.
func (s *Store) InsertTx(rec *TxRecord, block *BlockMeta) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	return scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		if block == nil {
			return s.insertMemPoolTx(ns, rec)
		}
		return s.insertMinedTx(ns, rec, block)
	})
}

// insertMinedTx inserts a new transaction record for a mined transaction into
// the database.  It is expected that the exact transation does not already
// exist in the unmined buckets, but unmined double spends (including mutations)
// are removed.
func (s *Store) insertMinedTx(ns walletdb.Bucket, rec *TxRecord,
	block *BlockMeta) error {
	// If a transaction record for this tx hash and block already exist,
	// there is nothing left to do.
	k, v := existsTxRecord(ns, &rec.Hash, &block.Block)
	if v != nil {
		return nil
	}

	// If the exact tx (not a double spend) is already included but
	// unconfirmed, move it to a block.
	v = existsRawUnmined(ns, rec.Hash[:])
	if v != nil {
		return s.moveMinedTx(ns, rec, k, v, block)
	}

	// As there may be unconfirmed transactions that are invalidated by this
	// transaction (either being duplicates, or double spends), remove them
	// from the unconfirmed set.  This also handles removing unconfirmed
	// transaction spend chains if any other unconfirmed transactions spend
	// outputs of the removed double spend.
	err := s.removeDoubleSpends(ns, rec)
	if err != nil {
		return err
	}

	// If a block record does not yet exist for any transactions from this
	// block, insert the record.  Otherwise, update it by adding the
	// transaction hash to the set of transactions from this block.
	blockKey, blockValue := existsBlockRecord(ns, block.Height)
	if blockValue == nil {
		err = putBlockRecord(ns, block, &rec.Hash)
	} else {
		blockValue, err = appendRawBlockRecord(blockValue, &rec.Hash)
		if err != nil {
			return err
		}
		err = putRawBlockRecord(ns, blockKey, blockValue)
	}
	if err != nil {
		return err
	}

	err = putTxRecord(ns, rec, &block.Block)
	if err != nil {
		return err
	}

	minedBalance, err := fetchMinedBalance(ns)
	if err != nil {
		return err
	}

	// Add a debit record for each unspent credit spent by this tx.
	spender := indexedIncidence{
		incidence: incidence{
			txHash: rec.Hash,
			block:  block.Block,
		},
		// index set for each iteration below
	}
	txType := stake.DetermineTxType(dcrutil.NewTx(&rec.MsgTx))

	for i, input := range rec.MsgTx.TxIn {
		unspentKey, credKey := existsUnspent(ns, &input.PreviousOutPoint)
		if credKey == nil {
			// Debits for unmined transactions are not explicitly
			// tracked.  Instead, all previous outputs spent by any
			// unmined transaction are added to a map for quick
			// lookups when it must be checked whether a mined
			// output is unspent or not.
			//
			// Tracking individual debits for unmined transactions
			// could be added later to simplify (and increase
			// performance of) determining some details that need
			// the previous outputs (e.g. determining a fee), but at
			// the moment that is not done (and a db lookup is used
			// for those cases instead).  There is also a good
			// chance that all unmined transaction handling will
			// move entirely to the db rather than being handled in
			// memory for atomicity reasons, so the simplist
			// implementation is currently used.
			continue
		}
		spender.index = uint32(i)
		amt, err := spendCredit(ns, credKey, &spender)
		if err != nil {
			return err
		}
		err = putDebit(ns, &rec.Hash, uint32(i), amt, &block.Block,
			credKey)
		if err != nil {
			return err
		}

		// Don't decrement spent ticket amounts.
		isTicketInput := (txType == stake.TxTypeSSGen && i == 1) ||
			(txType == stake.TxTypeSSRtx && i == 0)
		if !isTicketInput {
			minedBalance -= amt
		}

		err = deleteRawUnspent(ns, unspentKey)
		if err != nil {
			return err
		}
	}

	return putMinedBalance(ns, minedBalance)
}

// AddCredit marks a transaction record as containing a transaction output
// spendable by wallet.  The output is added unspent, and is marked spent
// when a new transaction spending the output is inserted into the store.
//
// TODO(jrick): This should not be necessary.  Instead, pass the indexes
// that are known to contain credits when a transaction or merkleblock is
// inserted into the store.
func (s *Store) AddCredit(rec *TxRecord, block *BlockMeta, index uint32,
	change bool) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if int(index) >= len(rec.MsgTx.TxOut) {
		str := "transaction output does not exist"
		return storeError(ErrInput, str, nil)
	}

	return scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		return s.addCredit(ns, rec, block, index, change)
	})
}

// getP2PKHOpCode returns OP_NONSTAKE for non-stake transactions, or
// the stake op code tag for stake transactions.
func getP2PKHOpCode(pkScript []byte) uint8 {
	class := txscript.GetScriptClass(txscript.DefaultScriptVersion, pkScript)
	switch {
	case class == txscript.StakeSubmissionTy:
		return txscript.OP_SSTX
	case class == txscript.StakeGenTy:
		return txscript.OP_SSGEN
	case class == txscript.StakeRevocationTy:
		return txscript.OP_SSRTX
	case class == txscript.StakeSubChangeTy:
		return txscript.OP_SSTXCHANGE
	}

	return OP_NONSTAKE
}

func (s *Store) addCredit(ns walletdb.Bucket, rec *TxRecord, block *BlockMeta,
	index uint32, change bool) error {
	opCode := getP2PKHOpCode(rec.MsgTx.TxOut[index].PkScript)
	isCoinbase := blockchain.IsCoinBaseTx(&rec.MsgTx)

	if block == nil {
		k := canonicalOutPoint(&rec.Hash, index)
		v := valueUnminedCredit(dcrutil.Amount(rec.MsgTx.TxOut[index].Value),
			change, opCode, isCoinbase)
		return putRawUnminedCredit(ns, k, v)
	}

	k, v := existsCredit(ns, &rec.Hash, index, &block.Block)
	if v != nil {
		return nil
	}

	txOutAmt := dcrutil.Amount(rec.MsgTx.TxOut[index].Value)
	log.Debugf("Marking transaction %v output %d (%v) spendable",
		rec.Hash, index, txOutAmt)

	cred := credit{
		outPoint: wire.OutPoint{
			Hash:  rec.Hash,
			Index: index,
		},
		block:      block.Block,
		amount:     txOutAmt,
		change:     change,
		spentBy:    indexedIncidence{index: ^uint32(0)},
		opCode:     opCode,
		isCoinbase: isCoinbase,
	}
	v = valueUnspentCredit(&cred)
	err := putRawCredit(ns, k, v)
	if err != nil {
		return err
	}

	minedBalance, err := fetchMinedBalance(ns)
	if err != nil {
		return err
	}
	// Update the balance so long as it's not a ticket output.
	if !(opCode == txscript.OP_SSTX) {
		err = putMinedBalance(ns, minedBalance+txOutAmt)
		if err != nil {
			return err
		}
	}

	return putUnspent(ns, &cred.outPoint, &block.Block)
}

// AddMultisigOut adds a P2SH multisignature spendable output into the
// transaction manager. In the event that the output already existed but
// was not mined, the output is updated so its value reflects the block
// it was included in.
//
func (s *Store) AddMultisigOut(rec *TxRecord, block *BlockMeta,
	index uint32) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if int(index) >= len(rec.MsgTx.TxOut) {
		str := "transaction output does not exist"
		return storeError(ErrInput, str, nil)
	}

	return scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		return s.addMultisigOut(ns, rec, block, index)
	})
}

func (s *Store) addMultisigOut(ns walletdb.Bucket, rec *TxRecord,
	block *BlockMeta, index uint32) error {
	empty := &chainhash.Hash{}

	// Check to see if the output already exists and is now being
	// mined into a block. If it does, update the record and return.
	key := keyMultisigOut(rec.Hash, index)
	val := existsMultisigOut(ns, key)
	if val != nil && block != nil {
		blockHashV, _ := fetchMultisigOutMined(val)
		if blockHashV.IsEqual(empty) {
			setMultisigOutMined(val, block.Block.Hash,
				uint32(block.Block.Height))
			putMultisigOutRawValues(ns, key, val)
			return nil
		} else {
			str := "tried to update a mined multisig out's mined information"
			return storeError(ErrDatabase, str, nil)
		}
	}
	// The multisignature output already exists in the database
	// as an unmined, unspent output and something is trying to
	// add it in duplicate. Return.
	if val != nil && block == nil {
		blockHashV, _ := fetchMultisigOutMined(val)
		if blockHashV.IsEqual(empty) {
			return nil
		}
	}

	// Dummy block for created transactions.
	if block == nil {
		block = &BlockMeta{Block{*empty, 0},
			rec.Received,
			0}
	}

	// Otherwise create a full record and insert it.
	p2shScript := rec.MsgTx.TxOut[index].PkScript
	class, _, _, err := txscript.ExtractPkScriptAddrs(
		rec.MsgTx.TxOut[index].Version, p2shScript, s.chainParams)
	tree := dcrutil.TxTreeRegular
	isStakeType := class == txscript.StakeSubmissionTy ||
		class == txscript.StakeSubChangeTy ||
		class == txscript.StakeGenTy ||
		class == txscript.StakeRevocationTy
	if isStakeType {
		class, err = txscript.GetStakeOutSubclass(p2shScript)
		if err != nil {
			str := "unknown stake output subclass encountered"
			return storeError(ErrInput, str, nil)
		}
		tree = dcrutil.TxTreeStake
	}
	if class != txscript.ScriptHashTy {
		str := "transaction output is wrong type (not p2sh)"
		return storeError(ErrInput, str, nil)
	}
	scriptHash, err :=
		txscript.GetScriptHashFromP2SHScript(p2shScript)
	if err != nil {
		return err
	}
	multisigScript := existsTxScript(ns, scriptHash)
	if multisigScript == nil {
		str := "failed to insert multisig out: transaction multisig " +
			"script does not exist in script bucket"
		return storeError(ErrNoExists, str, nil)
	}
	m, n, err := txscript.GetMultisigMandN(multisigScript)
	if err != nil {
		return storeError(ErrInput, err.Error(), nil)
	}
	var p2shScriptHash [ripemd160.Size]byte
	copy(p2shScriptHash[:], scriptHash)
	val = valueMultisigOut(p2shScriptHash,
		m,
		n,
		false,
		tree,
		block.Block.Hash,
		uint32(block.Block.Height),
		dcrutil.Amount(rec.MsgTx.TxOut[index].Value),
		*empty,     // Unspent
		0xFFFFFFFF, // Unspent
		rec.Hash)

	// Write the output, and insert the unspent key.
	err = putMultisigOutRawValues(ns, key, val)
	if err != nil {
		return storeError(ErrDatabase, err.Error(), nil)
	}
	return putMultisigOutUS(ns, key)
}

// SpendMultisigOut spends a multisignature output by making it spent in
// the general bucket and removing it from the unspent bucket.
func (s *Store) SpendMultisigOut(op *wire.OutPoint, spendHash chainhash.Hash,
	spendIndex uint32) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	return scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		return s.spendMultisigOut(ns, op, spendHash, spendIndex)
	})
}

func (s *Store) spendMultisigOut(ns walletdb.Bucket, op *wire.OutPoint,
	spendHash chainhash.Hash, spendIndex uint32) error {
	// Mark the output spent.
	key := keyMultisigOut(op.Hash, op.Index)
	val := existsMultisigOut(ns, key)
	if val == nil {
		str := "tried to spend multisig output that doesn't exist"
		return storeError(ErrNoExists, str, nil)
	}
	// Attempting to double spend an outpoint is an error.
	previouslyMarkedSpent := fetchMultisigOutSpent(val)
	if previouslyMarkedSpent {
		_, foundSpendHash, foundSpendIndex := fetchMultisigOutSpentVerbose(val)
		// It's not technically an error to try to respend
		// the output with exactly the same transaction.
		// However, there's no need to set it again. Just return.
		if spendHash.IsEqual(&foundSpendHash) && foundSpendIndex == spendIndex {
			return nil
		}
		str := "tried to doublespend multisig output"
		return storeError(ErrNoExists, str, nil)
	}
	setMultisigOutSpent(val, spendHash, spendIndex)

	// Check to see that it's in the unspent bucket.
	existsUnspent := existsMultisigOutUS(ns, key)
	if !existsUnspent {
		str := "unspent multisig outpoint is missing from the unspent bucket"
		return storeError(ErrInput, str, nil)
	}

	// Write the updated output, and delete the unspent key.
	err := putMultisigOutRawValues(ns, key, val)
	if err != nil {
		return storeError(ErrDatabase, err.Error(), nil)
	}
	err = deleteMultisigOutUS(ns, key)
	if err != nil {
		return storeError(ErrDatabase, err.Error(), nil)
	}

	return nil
}

// Rollback removes all blocks at height onwards, moving any transactions within
// each block to the unconfirmed pool.
func (s *Store) Rollback(height int32) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	return scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		return s.rollback(ns, height)
	})
}

// rollbackTransaction removes a transaction that was previously contained
// in a block during reorganization handling.
func (s *Store) rollbackTransaction(hash chainhash.Hash, b *blockRecord,
	coinBaseCredits *[]wire.OutPoint, minedBalance *dcrutil.Amount,
	ns walletdb.Bucket, isParent bool) error {
	txHash := &hash

	recKey := keyTxRecord(txHash, &b.Block)
	recVal := existsRawTxRecord(ns, recKey)
	var rec TxRecord
	err := readRawTxRecord(txHash, recVal, &rec)
	if err != nil {
		return err
	}

	err = deleteTxRecord(ns, txHash, &b.Block)
	if err != nil {
		return err
	}

	// If it's in the parent block, remove the tx hash from the
	// block entry.
	if isParent {
		blockKey, blockVal := existsBlockRecord(ns, b.Height)
		if blockVal == nil {
			return fmt.Errorf("couldn't find block %v", b.Height)
		}

		blockVal, err := removeRawBlockRecord(blockVal, txHash)
		if err != nil {
			return err
		}
		err = putRawBlockRecord(ns, blockKey, blockVal)
		if err != nil {
			return err
		}
	}

	// Handle coinbase transactions specially since they are
	// not moved to the unconfirmed store.  A coinbase cannot
	// contain any debits, but all credits should be removed
	// and the mined balance decremented.
	if blockchain.IsCoinBaseTx(&rec.MsgTx) {
		for i, output := range rec.MsgTx.TxOut {
			k, v := existsCredit(ns, &rec.Hash,
				uint32(i), &b.Block)
			if v == nil {
				continue
			}

			cbc := append(*coinBaseCredits, wire.OutPoint{rec.Hash,
				uint32(i), dcrutil.TxTreeRegular})
			coinBaseCredits = &cbc

			outPointKey := canonicalOutPoint(&rec.Hash, uint32(i))
			credKey := existsRawUnspent(ns, outPointKey)
			if credKey != nil {
				*minedBalance = *minedBalance - dcrutil.Amount(output.Value)
				err = deleteRawUnspent(ns, outPointKey)
				if err != nil {
					return err
				}
			}
			err = deleteRawCredit(ns, k)
			if err != nil {
				return err
			}

			// Check if this output is a multisignature
			// P2SH output. If it is, access the value
			// for the key and mark it unmined.
			msKey := keyMultisigOut(*txHash, uint32(i))
			msVal := existsMultisigOut(ns, msKey)
			if msVal != nil {
				setMultisigOutUnmined(msVal)
				err := putMultisigOutRawValues(ns, msKey, msVal)
				if err != nil {
					return err
				}
			}
		}

		return nil
	}

	err = putRawUnmined(ns, txHash[:], recVal)
	if err != nil {
		return err
	}

	txType := stake.DetermineTxType(dcrutil.NewTx(&rec.MsgTx))

	// For each debit recorded for this transaction, mark
	// the credit it spends as unspent (as long as it still
	// exists) and delete the debit.  The previous output is
	// recorded in the unconfirmed store for every previous
	// output, not just debits.
	for i, input := range rec.MsgTx.TxIn {
		// Skip stakebases.
		if i == 0 && txType == stake.TxTypeSSGen {
			continue
		}

		prevOut := &input.PreviousOutPoint
		prevOutKey := canonicalOutPoint(&prevOut.Hash,
			prevOut.Index)
		err = putRawUnminedInput(ns, prevOutKey, rec.Hash[:])
		if err != nil {
			return err
		}

		// If this input is a debit, remove the debit
		// record and mark the credit that it spent as
		// unspent, incrementing the mined balance.
		debKey, credKey, err := existsDebit(ns,
			&rec.Hash, uint32(i), &b.Block)
		if err != nil {
			return err
		}
		if debKey == nil {
			continue
		}

		// Store the credit OP code for later use.
		credVal := existsRawCredit(ns, credKey)
		if credVal == nil {
			return fmt.Errorf("missing credit value")
		}
		creditOpCode := fetchRawCreditTagOpCode(credVal)

		// unspendRawCredit does not error in case the
		// no credit exists for this key, but this
		// behavior is correct.  Since blocks are
		// removed in increasing order, this credit
		// may have already been removed from a
		// previously removed transaction record in
		// this rollback.
		var amt dcrutil.Amount
		amt, err = unspendRawCredit(ns, credKey)
		if err != nil {
			return err
		}

		err = deleteRawDebit(ns, debKey)
		if err != nil {
			return err
		}

		// If the credit was previously removed in the
		// rollback, the credit amount is zero.  Only
		// mark the previously spent credit as unspent
		// if it still exists.
		if amt == 0 {
			continue
		}
		unspentVal, err := fetchRawCreditUnspentValue(credKey)
		if err != nil {
			return err
		}

		// Ticket output spends are never decremented, so no need
		// to add them back.
		if !(creditOpCode == txscript.OP_SSTX) {
			*minedBalance = *minedBalance + amt
		}

		err = putRawUnspent(ns, prevOutKey, unspentVal)
		if err != nil {
			return err
		}

		// Check if this input uses a multisignature P2SH
		// output. If it did, mark the output unspent
		// and create an entry in the unspent bucket.
		msVal := existsMultisigOut(ns, prevOutKey)
		if msVal != nil {
			setMultisigOutUnSpent(msVal)
			err := putMultisigOutRawValues(ns, prevOutKey, msVal)
			if err != nil {
				return err
			}
			err = putMultisigOutUS(ns, prevOutKey)
			if err != nil {
				return err
			}
		}
	}

	// For each detached non-coinbase credit, move the
	// credit output to unmined.  If the credit is marked
	// unspent, it is removed from the utxo set and the
	// mined balance is decremented.
	//
	// TODO: use a credit iterator
	for i, output := range rec.MsgTx.TxOut {
		k, v := existsCredit(ns, &rec.Hash, uint32(i),
			&b.Block)
		if v == nil {
			continue
		}

		amt, change, err := fetchRawCreditAmountChange(v)
		if err != nil {
			return err
		}
		opCode := fetchRawCreditTagOpCode(v)
		isCoinbase := fetchRawCreditIsCoinbase(v)

		outPointKey := canonicalOutPoint(&rec.Hash, uint32(i))
		unminedCredVal := valueUnminedCredit(amt, change, opCode,
			isCoinbase)
		err = putRawUnminedCredit(ns, outPointKey, unminedCredVal)
		if err != nil {
			return err
		}

		err = deleteRawCredit(ns, k)
		if err != nil {
			return err
		}

		credKey := existsRawUnspent(ns, outPointKey)
		if credKey != nil {
			// Ticket amounts were never added, so ignore them when
			// correcting the balance.
			isTicketOutput := (txType == stake.TxTypeSStx && i == 0)
			if !isTicketOutput {
				*minedBalance = *minedBalance - dcrutil.Amount(output.Value)
			}
			err = deleteRawUnspent(ns, outPointKey)
			if err != nil {
				return err
			}
		}

		// Check if this output is a multisignature
		// P2SH output. If it is, access the value
		// for the key and mark it unmined.
		msKey := keyMultisigOut(*txHash, uint32(i))
		msVal := existsMultisigOut(ns, msKey)
		if msVal != nil {
			setMultisigOutUnmined(msVal)
			err := putMultisigOutRawValues(ns, msKey, msVal)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Store) rollback(ns walletdb.Bucket, height int32) error {
	minedBalanceWallet, err := fetchMinedBalance(ns)
	if err != nil {
		return err
	}

	minedBalance := new(dcrutil.Amount)
	*minedBalance = minedBalanceWallet

	// Keep track of all credits that were removed from coinbase
	// transactions.  After detaching all blocks, if any transaction record
	// exists in unmined that spends these outputs, remove them and their
	// spend chains.
	//
	// It is necessary to keep these in memory and fix the unmined
	// transactions later since blocks are removed in increasing order.
	cbcInitial := make([]wire.OutPoint, 0)
	coinBaseCredits := &cbcInitial

	topHeight, err := fetchChainHeight(ns, height)

	// This loop is inefficient; you end up getting most blocks twice and
	// redeserializing them from db. In the future, use a block iterator in
	// some intelligent way.
	for i := topHeight; i >= height; i-- {
		b, err := fetchBlockRecord(ns, i)
		if err != nil {
			return err
		}

		// Get parent too.
		pb, err := fetchBlockRecord(ns, i-1)
		if err != nil {
			return err
		}

		parentIsValid := dcrutil.IsFlagSet16(b.VoteBits,
			dcrutil.BlockValid)

		log.Debugf("Rolling back transactions from block %v height %d",
			b.Hash, b.Height)

		// Generate transaction list of transactions to remove from
		// both tx tree regular and tx tree stake. This can be done much
		// more efficiently if we stored the tx tree of transactions in
		// the txRecord so we don't have to deserialize the txRecords and
		// test them too.
		stakeTxFromBlock := make([]chainhash.Hash, 0)
		for _, hash := range b.transactions {
			// Super slow!
			txr, err := fetchTxRecord(ns, &hash, &Block{b.Hash, b.Height})
			if err != nil {
				return err
			}

			if stake.DetermineTxType(dcrutil.NewTx(&txr.MsgTx)) !=
				stake.TxTypeRegular {
				stakeTxFromBlock = append(stakeTxFromBlock, hash)
			}
		}

		regularTxFromParent := make([]chainhash.Hash, 0)
		if parentIsValid {
			for _, hash := range pb.transactions {
				// Super slow!
				txr, err := fetchTxRecord(ns, &hash, &Block{pb.Hash, pb.Height})
				if err != nil {
					return err
				}

				if stake.DetermineTxType(dcrutil.NewTx(&txr.MsgTx)) ==
					stake.TxTypeRegular {
					regularTxFromParent = append(regularTxFromParent, hash)
				}
			}
		}

		if parentIsValid {
			for _, hash := range regularTxFromParent {
				s.rollbackTransaction(hash, pb, coinBaseCredits, minedBalance,
					ns, true)
			}
		}
		for _, hash := range stakeTxFromBlock {
			s.rollbackTransaction(hash, b, coinBaseCredits, minedBalance, ns,
				false)
		}

		err = deleteBlockRecord(ns, i)
		if err != nil {
			return err
		}
	}

	for _, op := range *coinBaseCredits {
		opKey := canonicalOutPoint(&op.Hash, op.Index)
		unminedKey := existsRawUnminedInput(ns, opKey)
		if unminedKey != nil {
			unminedVal := existsRawUnmined(ns, unminedKey)
			var unminedRec TxRecord
			copy(unminedRec.Hash[:], unminedKey) // Silly but need an array
			err = readRawTxRecord(&unminedRec.Hash, unminedVal, &unminedRec)
			if err != nil {
				return err
			}

			log.Debugf("Transaction %v spends a removed coinbase "+
				"output -- removing as well", unminedRec.Hash)
			err = s.removeConflict(ns, &unminedRec)
			if err != nil {
				return err
			}
		}
	}

	return putMinedBalance(ns, *minedBalance)
}

// UnspentOutputs returns all unspent received transaction outputs.
// The order is undefined.
func (s *Store) UnspentOutputs() ([]*Credit, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var credits []*Credit
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		credits, err = s.unspentOutputs(ns)
		return err
	})
	return credits, err
}

func (s *Store) unspentOutputs(ns walletdb.Bucket) ([]*Credit, error) {
	var unspent []*Credit
	numUtxos := 0

	var op wire.OutPoint
	var block Block
	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip this k/v pair.
			return nil
		}
		err = readUnspentBlock(v, &block)
		if err != nil {
			return err
		}

		blockTime, err := fetchBlockTime(ns, block.Height)
		if err != nil {
			return err
		}
		// TODO(jrick): reading the entire transaction should
		// be avoidable.  Creating the credit only requires the
		// output amount and pkScript.
		rec, err := fetchTxRecord(ns, &op.Hash, &block)
		if err != nil {
			return err
		}
		txOut := rec.MsgTx.TxOut[op.Index]

		if stake.DetermineTxType(dcrutil.NewTx(&rec.MsgTx)) ==
			stake.TxTypeRegular {
			op.Tree = dcrutil.TxTreeRegular
		} else {
			op.Tree = dcrutil.TxTreeStake
		}

		cred := &Credit{
			OutPoint: op,
			BlockMeta: BlockMeta{
				Block: block,
				Time:  blockTime,
			},
			Amount:       dcrutil.Amount(txOut.Value),
			PkScript:     txOut.PkScript,
			Received:     rec.Received,
			FromCoinBase: blockchain.IsCoinBaseTx(&rec.MsgTx),
		}
		unspent = append(unspent, cred)
		numUtxos++

		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return nil, err
		}
		str := "failed iterating unspent bucket"
		return nil, storeError(ErrDatabase, str, err)
	}

	err = ns.Bucket(bucketUnminedCredits).ForEach(func(k, v []byte) error {
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip to next unmined credit.
			return nil
		}

		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}

		// TODO(jrick): Reading/parsing the entire transaction record
		// just for the output amount and script can be avoided.
		recVal := existsRawUnmined(ns, op.Hash[:])
		var rec TxRecord
		err = readRawTxRecord(&op.Hash, recVal, &rec)
		if err != nil {
			return err
		}

		if stake.DetermineTxType(dcrutil.NewTx(&rec.MsgTx)) ==
			stake.TxTypeRegular {
			op.Tree = dcrutil.TxTreeRegular
		} else {
			op.Tree = dcrutil.TxTreeStake
		}

		txOut := rec.MsgTx.TxOut[op.Index]
		cred := &Credit{
			OutPoint: op,
			BlockMeta: BlockMeta{
				Block: Block{Height: -1},
			},
			Amount:       dcrutil.Amount(txOut.Value),
			PkScript:     txOut.PkScript,
			Received:     rec.Received,
			FromCoinBase: blockchain.IsCoinBaseTx(&rec.MsgTx),
		}

		unspent = append(unspent, cred)
		numUtxos++

		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return nil, err
		}
		str := "failed iterating unmined credits bucket"
		return nil, storeError(ErrDatabase, str, err)
	}

	log.Tracef("%v many utxos found in database", numUtxos)

	return unspent, nil
}

// UnspentOutpoints returns all unspent received transaction outpoints.
// The order is undefined.
func (s *Store) UnspentOutpoints() ([]*wire.OutPoint, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var credits []*wire.OutPoint
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		credits, err = s.unspentOutpoints(ns)
		return err
	})
	return credits, err
}

func (s *Store) unspentOutpoints(ns walletdb.Bucket) ([]*wire.OutPoint, error) {
	var unspent []*wire.OutPoint
	numUtxos := 0

	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		var op wire.OutPoint
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip this k/v pair.
			return nil
		}

		block := new(Block)
		err = readUnspentBlock(v, block)
		if err != nil {
			return err
		}

		kC := keyCredit(&op.Hash, op.Index, block)
		vC := existsRawCredit(ns, kC)
		opCode := fetchRawCreditTagOpCode(vC)
		op.Tree = dcrutil.TxTreeRegular
		if opCode != OP_NONSTAKE {
			op.Tree = dcrutil.TxTreeStake
		}

		unspent = append(unspent, &op)
		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return nil, err
		}
		str := "failed iterating unspent bucket"
		return nil, storeError(ErrDatabase, str, err)
	}

	var unspentZC []*wire.OutPoint
	err = ns.Bucket(bucketUnminedCredits).ForEach(func(k, v []byte) error {
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip to next unmined credit.
			return nil
		}

		var op wire.OutPoint
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}

		opCode := fetchRawUnminedCreditTagOpcode(v)
		op.Tree = dcrutil.TxTreeRegular
		if opCode != OP_NONSTAKE {
			op.Tree = dcrutil.TxTreeStake
		}

		unspentZC = append(unspentZC, &op)
		numUtxos++

		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return nil, err
		}
		str := "failed iterating unmined credits bucket"
		return nil, storeError(ErrDatabase, str, err)
	}

	log.Tracef("%v many utxo outpoints found", numUtxos)

	return append(unspent, unspentZC...), nil
}

// UnspentTickets returns all unspent tickets that are known for this wallet.
// The order is undefined.
func (s *Store) UnspentTickets(syncHeight int32,
	includeImmature bool) ([]chainhash.Hash, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var tickets []chainhash.Hash
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		tickets, err = s.unspentTickets(ns, syncHeight, includeImmature)
		return err
	})
	return tickets, err
}

func (s *Store) unspentTickets(ns walletdb.Bucket, syncHeight int32,
	includeImmature bool) ([]chainhash.Hash, error) {
	var tickets []chainhash.Hash
	numTickets := 0

	var op wire.OutPoint
	var block Block
	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip this k/v pair.
			return nil
		}
		err = readUnspentBlock(v, &block)
		if err != nil {
			return err
		}

		kC := keyCredit(&op.Hash, op.Index, &block)
		vC := existsRawCredit(ns, kC)
		opCode := fetchRawCreditTagOpCode(vC)
		if opCode == txscript.OP_SSTX {
			if !includeImmature &&
				!confirmed(int32(s.chainParams.TicketMaturity)+1,
					block.Height, syncHeight) {
				return nil
			}
			tickets = append(tickets, op.Hash)
			numTickets++
		}

		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return nil, err
		}
		str := "failed iterating unspent bucket"
		return nil, storeError(ErrDatabase, str, err)
	}

	if includeImmature {
		err = ns.Bucket(bucketUnminedCredits).ForEach(func(k, v []byte) error {
			if existsRawUnminedInput(ns, k) != nil {
				// Output is spent by an unmined transaction.
				// Skip to next unmined credit.
				return nil
			}
			opCode := fetchRawUnminedCreditTagOpcode(v)
			if opCode == txscript.OP_SSTX {
				err := readCanonicalOutPoint(k, &op)
				if err != nil {
					return err
				}
				tickets = append(tickets, op.Hash)
				numTickets++
			}

			return nil
		})
		if err != nil {
			if _, ok := err.(Error); ok {
				return nil, err
			}
			str := "failed iterating unmined credits bucket"
			return nil, storeError(ErrDatabase, str, err)
		}
	}

	log.Tracef("%v many tickets found", numTickets)

	return tickets, nil
}

// MultisigCredit is a redeemable P2SH multisignature credit.
type MultisigCredit struct {
	OutPoint   *wire.OutPoint
	ScriptHash [ripemd160.Size]byte
	MSScript   []byte
	M          uint8
	N          uint8
	Amount     dcrutil.Amount
}

// GetMultisigCredit takes an outpoint and returns multisignature
// credit data stored about it.
func (s *Store) GetMultisigCredit(op *wire.OutPoint) (*MultisigCredit, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var credit *MultisigCredit
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		credit, err = s.getMultisigCredit(ns, op)
		return err
	})
	return credit, err
}

func (s *Store) getMultisigCredit(ns walletdb.Bucket,
	op *wire.OutPoint) (*MultisigCredit, error) {
	if op == nil {
		str := fmt.Sprintf("missing input outpoint")
		return nil, storeError(ErrInput, str, nil)
	}

	val := existsMultisigOut(ns, canonicalOutPoint(&op.Hash, op.Index))
	if val == nil {
		str := fmt.Sprintf("missing multisignature output for outpoint "+
			"hash %v, index %v (while getting ms credit)", op.Hash, op.Index)
		return nil, storeError(ErrNoExists, str, nil)
	}

	// Make sure it hasn't already been spent.
	spent, by, byIndex := fetchMultisigOutSpentVerbose(val)
	if spent {
		str := fmt.Sprintf("multisignature output %v index %v has already "+
			"been spent by transaction %v (input %v)", op.Hash, op.Index,
			by, byIndex)
		return nil, storeError(ErrInput, str, nil)
	}

	// Script is contained in val above too, but I check this
	// to make sure the db has consistency.
	scriptHash := fetchMultisigOutScrHash(val)
	multisigScript := existsTxScript(ns, scriptHash[:])
	if multisigScript == nil {
		str := "couldn't get multisig credit: transaction multisig " +
			"script does not exist in script bucket"
		return nil, storeError(ErrNoExists, str, nil)
	}
	m, n := fetchMultisigOutMN(val)
	amount := fetchMultisigOutAmount(val)
	op.Tree = fetchMultisigOutTree(val)

	msc := &MultisigCredit{
		op,
		scriptHash,
		multisigScript,
		m,
		n,
		amount,
	}

	return msc, nil
}

// GetMultisigOutput takes an outpoint and returns multisignature
// credit data stored about it.
func (s *Store) GetMultisigOutput(op *wire.OutPoint) (*MultisigOut, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var credit *MultisigOut
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		credit, err = s.getMultisigOutput(ns, op)
		return err
	})
	return credit, err
}

func (s *Store) getMultisigOutput(ns walletdb.Bucket,
	op *wire.OutPoint) (*MultisigOut, error) {
	if op == nil {
		str := fmt.Sprintf("missing input outpoint")
		return nil, storeError(ErrInput, str, nil)
	}

	key := canonicalOutPoint(&op.Hash, op.Index)
	val := existsMultisigOut(ns, key)
	if val == nil {
		str := fmt.Sprintf("missing multisignature output for outpoint "+
			"hash %v, index %v", op.Hash, op.Index)
		return nil, storeError(ErrNoExists, str, nil)
	}

	mso, err := fetchMultisigOut(key, val)
	if err != nil {
		str := fmt.Sprintf("failed to deserialized multisignature output "+
			"for outpoint hash %v, index %v", op.Hash, op.Index)
		return nil, storeError(ErrNoExists, str, nil)
	}

	return mso, nil
}

// UnspentMultisigCredits returns all unspent multisignature P2SH credits in
// the wallet.
func (s *Store) UnspentMultisigCredits() ([]*MultisigCredit, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var credits []*MultisigCredit
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		credits, err = s.unspentMultisigCredits(ns)
		return err
	})
	return credits, err
}

func (s *Store) unspentMultisigCredits(ns walletdb.Bucket) ([]*MultisigCredit,
	error) {
	var unspentKeys [][]byte

	err := ns.Bucket(bucketMultisigUsp).ForEach(func(k, v []byte) error {
		unspentKeys = append(unspentKeys, k)
		return nil
	})

	var mscs []*MultisigCredit
	for _, key := range unspentKeys {
		val := existsMultisigOut(ns, key)
		if val == nil {
			str := "failed to get unspent multisig credits: " +
				"does not exist in bucket"
			return nil, storeError(ErrNoExists, str, nil)
		}
		var op wire.OutPoint
		errRead := readCanonicalOutPoint(key, &op)
		if errRead != nil {
			return nil, storeError(ErrInput, errRead.Error(), err)
		}

		scriptHash := fetchMultisigOutScrHash(val)
		multisigScript := existsTxScript(ns, scriptHash[:])
		if multisigScript == nil {
			str := "failed to get unspent multisig credits: " +
				"transaction multisig script does not exist " +
				"in script bucket"
			return nil, storeError(ErrNoExists, str, nil)
		}
		m, n := fetchMultisigOutMN(val)
		amount := fetchMultisigOutAmount(val)
		op.Tree = fetchMultisigOutTree(val)

		msc := &MultisigCredit{
			&op,
			scriptHash,
			multisigScript,
			m,
			n,
			amount,
		}
		mscs = append(mscs, msc)
	}

	return mscs, nil
}

// UnspentMultisigCreditsForAddress returns all unspent multisignature P2SH
// credits in the wallet for some specified address.
func (s *Store) UnspentMultisigCreditsForAddress(
	addr dcrutil.Address) ([]*MultisigCredit, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var credits []*MultisigCredit
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		credits, err = s.unspentMultisigCreditsForAddress(ns, addr)
		return err
	})
	return credits, err
}

func (s *Store) unspentMultisigCreditsForAddress(ns walletdb.Bucket,
	addr dcrutil.Address) ([]*MultisigCredit, error) {
	// Make sure the address is P2SH, then get the
	// Hash160 for the script from the address.
	var addrScrHash []byte
	if sha, ok := addr.(*dcrutil.AddressScriptHash); ok {
		addrScrHash = sha.ScriptAddress()
	} else {
		str := "address passed was not a P2SH address"
		return nil, storeError(ErrInput, str, nil)
	}

	var unspentKeys [][]byte
	err := ns.Bucket(bucketMultisigUsp).ForEach(func(k, v []byte) error {
		unspentKeys = append(unspentKeys, k)
		return nil
	})

	var mscs []*MultisigCredit
	for _, key := range unspentKeys {
		val := existsMultisigOut(ns, key)
		if val == nil {
			str := "failed to get unspent multisig credits: " +
				"does not exist in bucket"
			return nil, storeError(ErrNoExists, str, nil)
		}

		// Skip everything that's unrelated to the address
		// we're concerned about.
		scriptHash := fetchMultisigOutScrHash(val)
		if !bytes.Equal(scriptHash[:], addrScrHash) {
			continue
		}

		var op wire.OutPoint
		errRead := readCanonicalOutPoint(key, &op)
		if errRead != nil {
			return nil, storeError(ErrInput, errRead.Error(), err)
		}

		multisigScript := existsTxScript(ns, scriptHash[:])
		if multisigScript == nil {
			str := "failed to get unspent multisig credits: " +
				"transaction multisig script does not exist " +
				"in script bucket"
			return nil, storeError(ErrNoExists, str, nil)
		}
		m, n := fetchMultisigOutMN(val)
		amount := fetchMultisigOutAmount(val)
		op.Tree = fetchMultisigOutTree(val)

		msc := &MultisigCredit{
			&op,
			scriptHash,
			multisigScript,
			m,
			n,
			amount,
		}
		mscs = append(mscs, msc)
	}

	return mscs, nil
}

// UnspentOutputsForAmount returns all non-stake outputs that sum up to the
// amount passed. If not enough funds are found, a nil pointer is returned
// without error.
func (s *Store) UnspentOutputsForAmount(amt dcrutil.Amount, height int32,
	minConf int32) ([]*Credit, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var credits []*Credit
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		credits, err = s.unspentOutputsForAmount(ns, amt, height, minConf)
		return err
	})
	return credits, err
}

type minimalCredit struct {
	txRecordKey []byte
	index       uint32
	Amount      int64
	tree        int8
	unmined     bool
}

// ByUtxoAmount defines the methods needed to satisify sort.Interface to
// sort a slice of Utxos by their amount.
type ByUtxoAmount []*minimalCredit

func (u ByUtxoAmount) Len() int           { return len(u) }
func (u ByUtxoAmount) Less(i, j int) bool { return u[i].Amount < u[j].Amount }
func (u ByUtxoAmount) Swap(i, j int)      { u[i], u[j] = u[j], u[i] }

// confirmed checks whether a transaction at height txHeight has met minConf
// confirmations for a blockchain at height curHeight.
func confirmed(minConf, txHeight, curHeight int32) bool {
	return confirms(txHeight, curHeight) >= minConf
}

// confirms returns the number of confirmations for a transaction in a block at
// height txHeight (or -1 for an unconfirmed tx) given the chain height
// curHeight.
func confirms(txHeight, curHeight int32) int32 {
	switch {
	case txHeight == -1, txHeight > curHeight:
		return 0
	default:
		return curHeight - txHeight + 1
	}
}

// forEachBreakout is used to break out of a a wallet db ForEach loop.
var forEachBreakout = errors.New("forEachBreakout")

func (s *Store) unspentOutputsForAmount(ns walletdb.Bucket, needed dcrutil.Amount,
	syncHeight int32, minConf int32) ([]*Credit, error) {
	var eligible []*minimalCredit
	var toUse []*minimalCredit
	var unspent []*Credit
	found := dcrutil.Amount(0)

	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		if found >= needed {
			return forEachBreakout
		}

		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip to next unmined credit.
			return nil
		}

		cKey := make([]byte, 72)
		copy(cKey[0:32], k[0:32])   // Tx hash
		copy(cKey[32:36], v[0:4])   // Block height
		copy(cKey[36:68], v[4:36])  // Block hash
		copy(cKey[68:72], k[32:36]) // Output index

		cVal := existsRawCredit(ns, cKey)
		if cVal == nil {
			return nil
		}

		amt, spent, err := fetchRawCreditAmountSpent(cVal)
		if err != nil {
			return err
		}

		// This should never happen since this is already in bucket
		// unspent, but let's be careful anyway.
		if spent {
			return nil
		}
		// Skip ticket outputs, as only SSGen can spend these.
		opcode := fetchRawCreditTagOpCode(cVal)
		if opcode == txscript.OP_SSTX {
			return nil
		}

		// Only include this output if it meets the required number of
		// confirmations.  Coinbase transactions must have have reached
		// maturity before their outputs may be spent.
		txHeight := extractRawCreditHeight(cKey)
		if !confirmed(minConf, txHeight, syncHeight) {
			return nil
		}

		// Skip outputs that are not mature.
		if opcode == OP_NONSTAKE && fetchRawCreditIsCoinbase(cVal) {
			if !confirmed(int32(s.chainParams.CoinbaseMaturity), txHeight,
				syncHeight) {
				return nil
			}
		}
		if opcode == txscript.OP_SSGEN || opcode == txscript.OP_SSRTX {
			if !confirmed(int32(s.chainParams.CoinbaseMaturity), txHeight,
				syncHeight) {
				return nil
			}
		}
		if opcode == txscript.OP_SSTXCHANGE {
			if !confirmed(int32(s.chainParams.SStxChangeMaturity), txHeight,
				syncHeight) {
				return nil
			}
		}

		// Determine the txtree for the outpoint by whether or not it's
		// using stake tagged outputs.
		tree := dcrutil.TxTreeRegular
		if opcode != OP_NONSTAKE {
			tree = dcrutil.TxTreeStake
		}

		mc := &minimalCredit{
			extractRawCreditTxRecordKey(cKey),
			extractRawCreditIndex(cKey),
			int64(amt),
			tree,
			false,
		}

		eligible = append(eligible, mc)
		found += amt

		return nil
	})
	if err != nil {
		if err != forEachBreakout {
			if _, ok := err.(Error); ok {
				return nil, err
			}
			str := "failed iterating unspent bucket"
			return nil, storeError(ErrDatabase, str, err)
		}
	}

	// Unconfirmed transaction output handling.
	if minConf == 0 {
		err = ns.Bucket(bucketUnminedCredits).ForEach(func(k, v []byte) error {
			if found >= needed {
				return forEachBreakout
			}

			// Make sure this output was not spent by an unmined transaction.
			// If it was, skip this credit.
			if existsRawUnminedInput(ns, k) != nil {
				return nil
			}

			amt, err := fetchRawUnminedCreditAmount(v)
			if err != nil {
				return err
			}

			// Skip ticket outputs, as only SSGen can spend these.
			opcode := fetchRawUnminedCreditTagOpcode(v)
			if opcode == txscript.OP_SSTX {
				return nil
			}

			// Skip outputs that are not mature.
			if opcode == txscript.OP_SSGEN || opcode == txscript.OP_SSRTX {
				return nil
			}
			if opcode == txscript.OP_SSTXCHANGE {
				return nil
			}

			// Determine the txtree for the outpoint by whether or not it's
			// using stake tagged outputs.
			tree := dcrutil.TxTreeRegular
			if opcode != OP_NONSTAKE {
				tree = dcrutil.TxTreeStake
			}

			localOp := new(wire.OutPoint)
			err = readCanonicalOutPoint(k, localOp)
			if err != nil {
				return err
			}

			mc := &minimalCredit{
				localOp.Hash[:],
				localOp.Index,
				int64(amt),
				tree,
				true,
			}

			eligible = append(eligible, mc)
			found += amt

			return nil
		})
	}
	if err != nil {
		if err != forEachBreakout {
			if _, ok := err.(Error); ok {
				return nil, err
			}
			str := "failed iterating unmined credits bucket"
			return nil, storeError(ErrDatabase, str, err)
		}
	}

	// Sort by amount, descending.
	sort.Sort(sort.Reverse(ByUtxoAmount(eligible)))

	sum := int64(0)
	for _, mc := range eligible {
		toUse = append(toUse, mc)
		sum += mc.Amount

		// Exit the loop if we have enough outputs.
		if sum >= int64(needed) {
			break
		}
	}

	// We couldn't find enough utxos to possibly generate an output
	// of needed, so just return.
	if sum < int64(needed) {
		return nil, nil
	}

	// Look up the Credit data we need for our utxo and store it.
	for _, mc := range toUse {
		switch mc.unmined {
		case false:
			opHash, err := chainhash.NewHash(mc.txRecordKey[0:32])
			if err != nil {
				return nil, err
			}

			var block Block
			err = readUnspentBlock(mc.txRecordKey[32:68], &block)
			if err != nil {
				return nil, err
			}

			blockTime, err := fetchBlockTime(ns, block.Height)
			if err != nil {
				return nil, err
			}

			// TODO(jrick): reading the entire transaction should
			// be avoidable.  Creating the credit only requires the
			// output amount and pkScript.
			rec, err := fetchTxRecord(ns, opHash, &block)
			if err != nil {
				return nil, err
			}

			txOut := rec.MsgTx.TxOut[mc.index]
			cred := &Credit{
				OutPoint: wire.OutPoint{
					*opHash,
					mc.index,
					mc.tree,
				},
				BlockMeta: BlockMeta{
					Block: block,
					Time:  blockTime,
				},
				Amount:       dcrutil.Amount(txOut.Value),
				PkScript:     txOut.PkScript,
				Received:     rec.Received,
				FromCoinBase: blockchain.IsCoinBaseTx(&rec.MsgTx),
			}
			unspent = append(unspent, cred)

		case true:
			localOp := new(wire.OutPoint)
			copy(localOp.Hash[:], mc.txRecordKey[:])
			localOp.Index = mc.index

			unminedV := existsRawUnmined(ns, mc.txRecordKey)
			if unminedV == nil {
				return nil, fmt.Errorf("couldn't find unmined tx %x in "+
					"unmined bucket",
					mc.txRecordKey)
			}

			localMsgTx := new(wire.MsgTx)
			err = localMsgTx.Deserialize(bytes.NewReader(unminedV[8:]))
			if err != nil {
				return nil, err
			}
			txOut := localMsgTx.TxOut[mc.index]

			cred := &Credit{
				OutPoint: wire.OutPoint{
					localMsgTx.TxSha(),
					mc.index,
					mc.tree,
				},
				BlockMeta:    BlockMeta{},
				Amount:       dcrutil.Amount(mc.Amount),
				PkScript:     txOut.PkScript,
				Received:     time.Now(),
				FromCoinBase: false,
			}

			unspent = append(unspent, cred)
		}
	}

	return unspent, nil
}

// Balance returns the spendable wallet balance (total value of all unspent
// transaction outputs) given a minimum of minConf confirmations, calculated
// at a current chain height of curHeight.  Coinbase outputs are only included
// in the balance if maturity has been reached.
//
// Balance may return unexpected results if syncHeight is lower than the block
// height of the most recent mined transaction in the store.
func (s *Store) Balance(minConf, syncHeight int32,
	balanceType BehaviorFlags) (dcrutil.Amount, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return 0, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var amt dcrutil.Amount
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		amt, err = s.balance(ns, minConf, syncHeight, balanceType)
		return err
	})
	return amt, err
}

func (s *Store) balance(ns walletdb.Bucket, minConf int32,
	syncHeight int32, balanceType BehaviorFlags) (dcrutil.Amount, error) {
	switch balanceType {
	case BFBalanceFullScan:
		return s.balanceFullScan(ns, minConf, syncHeight)
	case BFBalanceSpendable:
		return s.balanceSpendable(ns, minConf, syncHeight)
	case BFBalanceLockedStake:
		return s.balanceLockedStake(ns, minConf, syncHeight)
	case BFBalanceAll:
		return s.balanceAll(ns, minConf, syncHeight)
	default:
		return 0, fmt.Errorf("unknown balance type flag")
	}
}

// balanceSpendable is the current spendable balance of the wallet.
func (s *Store) balanceSpendable(ns walletdb.Bucket, minConf int32,
	syncHeight int32) (dcrutil.Amount, error) {
	bal, err := fetchMinedBalance(ns)
	if err != nil {
		return 0, err
	}

	// Subtract the balance for each credit that is spent by an unmined
	// transaction, except for those spending tickets.
	var op wire.OutPoint
	var block Block
	err = ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}
		err = readUnspentBlock(v, &block)
		if err != nil {
			return err
		}
		if existsRawUnminedInput(ns, k) != nil {
			_, v := existsCredit(ns, &op.Hash, op.Index, &block)
			amt, err := fetchRawCreditAmount(v)
			if err != nil {
				return err
			}
			opCode := fetchRawCreditTagOpCode(v)
			if opCode != txscript.OP_SSTX {
				bal -= amt
			}
		}
		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return 0, err
		}
		str := "failed iterating unspent outputs"
		return 0, storeError(ErrDatabase, str, err)
	}

	// Decrement the balance for any unspent credit with less than
	// minConf confirmations and any (unspent) immature coinbase credit.
	stopConf := minConf
	if int32(s.chainParams.CoinbaseMaturity) > stopConf {
		stopConf = int32(s.chainParams.CoinbaseMaturity)
	}
	lastHeight := syncHeight - stopConf
	blockIt := makeReverseBlockIterator(ns)
	for blockIt.prev() {
		blockIter := &blockIt.elem

		if blockIter.Height < lastHeight {
			break
		}
		for i := range blockIter.transactions {
			txHash := &blockIter.transactions[i]
			rec, err := fetchTxRecord(ns, txHash, &blockIter.Block)
			if err != nil {
				return 0, err
			}
			numOuts := uint32(len(rec.MsgTx.TxOut))
			for i := uint32(0); i < numOuts; i++ {
				// Avoid double decrementing the credit amount
				// if it was already removed for being spent by
				// an unmined tx.
				opKey := canonicalOutPoint(txHash, i)
				if existsRawUnminedInput(ns, opKey) != nil {
					continue
				}

				_, v := existsCredit(ns, txHash, i, &blockIter.Block)
				if v == nil {
					continue
				}
				opcode := fetchRawCreditTagOpCode(v)
				amt, spent, err := fetchRawCreditAmountSpent(v)
				if err != nil {
					return 0, err
				}
				if spent {
					continue
				}

				switch {
				case opcode == OP_NONSTAKE:
					if !confirmed(minConf, blockIter.Height, syncHeight) {
						bal -= amt
						continue
					}

					immatureCoinbase := (blockchain.IsCoinBaseTx(&rec.MsgTx) &&
						!confirmed(int32(s.chainParams.CoinbaseMaturity),
							blockIter.Height,
							syncHeight))
					if immatureCoinbase {
						bal -= amt
						continue
					}

				case opcode == txscript.OP_SSTX:
					// Locked as stake ticket. These were never added to the
					// balance in the first place, so ignore them.
				case opcode == txscript.OP_SSGEN:
					if !confirmed(int32(s.chainParams.CoinbaseMaturity),
						blockIter.Height, syncHeight) {
						bal -= amt
					}
				case opcode == txscript.OP_SSRTX:
					if !confirmed(int32(s.chainParams.CoinbaseMaturity),
						blockIter.Height, syncHeight) {
						bal -= amt
					}
				case opcode == txscript.OP_SSTXCHANGE:
					if !confirmed(int32(s.chainParams.SStxChangeMaturity),
						blockIter.Height, syncHeight) {
						bal -= amt
					}
				}
			}
		}
	}
	if blockIt.err != nil {
		return 0, blockIt.err
	}

	// If unmined outputs are included, increment the balance for each
	// output that is unspent.
	if minConf == 0 {
		err = ns.Bucket(bucketUnminedCredits).ForEach(func(k, v []byte) error {
			if existsRawUnminedInput(ns, k) != nil {
				// Output is spent by an unmined transaction.
				// Skip to next unmined credit.
				return nil
			}

			amount, err := fetchRawUnminedCreditAmount(v)
			if err != nil {
				return err
			}
			opcode := fetchRawCreditTagOpCode(v)
			if opcode == OP_NONSTAKE {
				bal += amount
			}
			return nil
		})
		if err != nil {
			if _, ok := err.(Error); ok {
				return 0, err
			}
			str := "failed to iterate over unmined credits bucket"
			return 0, storeError(ErrDatabase, str, err)
		}
	}
	return bal, nil
}

// balanceSpendableSimulated is a simulated version of the balanceSpendable
// function that allows you to verify the integrity of a balance after
// performing a rollback by using an old bucket of unmined inputs.
// Use only with minconf>0.
func (s *Store) balanceSpendableSimulated(ns walletdb.Bucket, minConf int32,
	syncHeight int32, unminedInputs map[string][]byte) (dcrutil.Amount, error) {
	bal, err := fetchMinedBalance(ns)
	if err != nil {
		return 0, err
	}

	// Subtract the balance for each credit that is spent by an unmined
	// transaction, except for those spending tickets.
	var op wire.OutPoint
	var block Block
	err = ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}
		err = readUnspentBlock(v, &block)
		if err != nil {
			return err
		}

		kStr := hex.EncodeToString(k)
		_, ok := unminedInputs[kStr]
		if ok {
			_, v := existsCredit(ns, &op.Hash, op.Index, &block)
			amt, err := fetchRawCreditAmount(v)
			if err != nil {
				return err
			}
			opCode := fetchRawCreditTagOpCode(v)
			if opCode != txscript.OP_SSTX {
				bal -= amt
			}
		}
		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return 0, err
		}
		str := "failed iterating unspent outputs"
		return 0, storeError(ErrDatabase, str, err)
	}

	// Decrement the balance for any unspent credit with less than
	// minConf confirmations and any (unspent) immature coinbase credit.
	stopConf := minConf
	if int32(s.chainParams.CoinbaseMaturity) > stopConf {
		stopConf = int32(s.chainParams.CoinbaseMaturity)
	}
	lastHeight := syncHeight - stopConf
	blockIt := makeReverseBlockIterator(ns)
	for blockIt.prev() {
		blockIter := &blockIt.elem

		if blockIter.Height < lastHeight {
			break
		}
		for i := range blockIter.transactions {
			txHash := &blockIter.transactions[i]
			rec, err := fetchTxRecord(ns, txHash, &blockIter.Block)
			if err != nil {
				return 0, err
			}
			numOuts := uint32(len(rec.MsgTx.TxOut))
			for i := uint32(0); i < numOuts; i++ {
				// Avoid double decrementing the credit amount
				// if it was already removed for being spent by
				// an unmined tx.
				opKey := canonicalOutPoint(txHash, i)
				kStr := hex.EncodeToString(opKey)
				_, ok := unminedInputs[kStr]
				if ok {
					continue
				}

				_, v := existsCredit(ns, txHash, i, &blockIter.Block)
				if v == nil {
					continue
				}
				opcode := fetchRawCreditTagOpCode(v)
				amt, spent, err := fetchRawCreditAmountSpent(v)
				if err != nil {
					return 0, err
				}
				if spent {
					continue
				}

				switch {
				case opcode == OP_NONSTAKE:
					if !confirmed(minConf, blockIter.Height, syncHeight) {
						bal -= amt
						continue
					}

					immatureCoinbase := (blockchain.IsCoinBaseTx(&rec.MsgTx) &&
						!confirmed(int32(s.chainParams.CoinbaseMaturity),
							blockIter.Height,
							syncHeight))
					if immatureCoinbase {
						bal -= amt
						continue
					}

				case opcode == txscript.OP_SSTX:
					// Locked as stake ticket. These were never added to the
					// balance in the first place, so ignore them.
				case opcode == txscript.OP_SSGEN:
					if !confirmed(int32(s.chainParams.CoinbaseMaturity),
						blockIter.Height, syncHeight) {
						bal -= amt
					}
				case opcode == txscript.OP_SSRTX:
					if !confirmed(int32(s.chainParams.CoinbaseMaturity),
						blockIter.Height, syncHeight) {
						bal -= amt
					}
				case opcode == txscript.OP_SSTXCHANGE:
					if !confirmed(int32(s.chainParams.SStxChangeMaturity),
						blockIter.Height, syncHeight) {
						bal -= amt
					}
				}
			}
		}
	}
	if blockIt.err != nil {
		return 0, blockIt.err
	}

	return bal, nil
}

// balanceLockedStake returns the current balance of the wallet that is locked
// in tickets.
func (s *Store) balanceLockedStake(ns walletdb.Bucket, minConf int32,
	syncHeight int32) (dcrutil.Amount, error) {
	var amt dcrutil.Amount
	var op wire.OutPoint

	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip to next unmined credit.
			return nil
		}

		cKey := make([]byte, 72)
		copy(cKey[0:32], k[0:32])   // Tx hash
		copy(cKey[32:36], v[0:4])   // Block height
		copy(cKey[36:68], v[4:36])  // Block hash
		copy(cKey[68:72], k[32:36]) // Output index

		// Skip unmined credits.
		cVal := existsRawCredit(ns, cKey)
		if cVal == nil {
			return nil
		}

		// Skip non-ticket outputs, as only SSGen can spend these.
		opcode := fetchRawCreditTagOpCode(cVal)
		if opcode != txscript.OP_SSTX {
			return nil
		}

		amtCredit, spent, err := fetchRawCreditAmountSpent(cVal)
		if err != nil {
			return err
		}

		if spent {
			return fmt.Errorf("spent credit found in unspent bucket")
		}

		amt += amtCredit

		return nil
	})
	return amt, err
}

// balanceFullScan does a fullscan of the UTXO to determine a 1 conf or
// greater balance. Mostly intended to be used as a debugging function;
// it should return the same balance of balanceSpendable for minconf > 0.
func (s *Store) balanceFullScan(ns walletdb.Bucket, minConf int32,
	syncHeight int32) (dcrutil.Amount, error) {
	if minConf <= 0 {
		return 0, storeError(ErrInput, "0 or negative minconf given "+
			"for fullscan request", nil)
	}

	var amt dcrutil.Amount

	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip to next unmined credit.
			return nil
		}

		cKey := make([]byte, 72)
		copy(cKey[0:32], k[0:32])   // Tx hash
		copy(cKey[32:36], v[0:4])   // Block height
		copy(cKey[36:68], v[4:36])  // Block hash
		copy(cKey[68:72], k[32:36]) // Output index

		cVal := existsRawCredit(ns, cKey)
		if cVal == nil {
			return fmt.Errorf("couldn't find a credit for unspent txo")
		}

		utxoAmt, err := fetchRawCreditAmount(cVal)
		if err != nil {
			return err
		}

		height := extractRawCreditHeight(cKey)
		opcode := fetchRawCreditTagOpCode(cVal)

		switch {
		case opcode == OP_NONSTAKE:
			isConfirmed := confirmed(minConf, height, syncHeight)
			creditFromCoinbase := fetchRawCreditIsCoinbase(cVal)
			matureCoinbase := (creditFromCoinbase &&
				confirmed(int32(s.chainParams.CoinbaseMaturity),
					height,
					syncHeight))

			if isConfirmed && !creditFromCoinbase {
				amt += utxoAmt
			}

			if creditFromCoinbase && matureCoinbase {
				amt += utxoAmt
			}

		case opcode == txscript.OP_SSTX:
			// amt += utxoAmt
			// Locked as stake ticket. These were never added to the
			// balance in the first place, so ignore them.
		case opcode == txscript.OP_SSGEN:
			if confirmed(int32(s.chainParams.CoinbaseMaturity),
				height, syncHeight) {
				amt += utxoAmt
			}

		case opcode == txscript.OP_SSRTX:
			if confirmed(int32(s.chainParams.CoinbaseMaturity),
				height, syncHeight) {
				amt += utxoAmt
			}
		case opcode == txscript.OP_SSTXCHANGE:
			if confirmed(int32(s.chainParams.SStxChangeMaturity),
				height, syncHeight) {
				amt += utxoAmt
			}
		}

		return nil
	})
	return amt, err
}

// balanceFullScanSimulated is a simulated version of the balanceFullScan
// function that allows you to verify the integrity of a balance after
// performing a rollback by using an old bucket of unmined inputs.
// Use only with minconf>0.
func (s *Store) balanceFullScanSimulated(ns walletdb.Bucket, minConf int32,
	syncHeight int32, unminedInputs map[string][]byte) (dcrutil.Amount, error) {
	if minConf <= 0 {
		return 0, storeError(ErrInput, "0 or negative minconf given "+
			"for fullscan request", nil)
	}

	var amt dcrutil.Amount

	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		_, ok := unminedInputs[kStr]
		if ok {
			// Output is spent by an unmined transaction.
			// Skip to next unmined credit.
			return nil
		}

		cKey := make([]byte, 72)
		copy(cKey[0:32], k[0:32])   // Tx hash
		copy(cKey[32:36], v[0:4])   // Block height
		copy(cKey[36:68], v[4:36])  // Block hash
		copy(cKey[68:72], k[32:36]) // Output index

		cVal := existsRawCredit(ns, cKey)
		if cVal == nil {
			return fmt.Errorf("couldn't find a credit for unspent txo")
		}

		utxoAmt, err := fetchRawCreditAmount(cVal)
		if err != nil {
			return err
		}

		height := extractRawCreditHeight(cKey)
		opcode := fetchRawCreditTagOpCode(cVal)

		switch {
		case opcode == OP_NONSTAKE:
			isConfirmed := confirmed(minConf, height, syncHeight)
			creditFromCoinbase := fetchRawCreditIsCoinbase(cVal)
			matureCoinbase := (creditFromCoinbase &&
				confirmed(int32(s.chainParams.CoinbaseMaturity),
					height,
					syncHeight))

			if isConfirmed && !creditFromCoinbase {
				amt += utxoAmt
			}

			if creditFromCoinbase && matureCoinbase {
				amt += utxoAmt
			}

		case opcode == txscript.OP_SSTX:
			// Locked as stake ticket. These were never added to the
			// balance in the first place, so ignore them.
		case opcode == txscript.OP_SSGEN:
			if confirmed(int32(s.chainParams.CoinbaseMaturity),
				height, syncHeight) {
				amt += utxoAmt
			}

		case opcode == txscript.OP_SSRTX:
			if confirmed(int32(s.chainParams.CoinbaseMaturity),
				height, syncHeight) {
				amt += utxoAmt
			}
		case opcode == txscript.OP_SSTXCHANGE:
			if confirmed(int32(s.chainParams.SStxChangeMaturity),
				height, syncHeight) {
				amt += utxoAmt
			}
		}

		return nil
	})
	return amt, err
}

// balanceAll returns the balance of all unspent outputs that are not spent
// by unmined transactions.
func (s *Store) balanceAll(ns walletdb.Bucket, minConf int32,
	syncHeight int32) (dcrutil.Amount, error) {
	var amt dcrutil.Amount
	var op wire.OutPoint

	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		err := readCanonicalOutPoint(k, &op)
		if err != nil {
			return err
		}
		if existsRawUnminedInput(ns, k) != nil {
			// Output is spent by an unmined transaction.
			// Skip to next unmined credit.
			return nil
		}

		cKey := make([]byte, 72)
		copy(cKey[0:32], k[0:32])   // Tx hash
		copy(cKey[32:36], v[0:4])   // Block height
		copy(cKey[36:68], v[4:36])  // Block hash
		copy(cKey[68:72], k[32:36]) // Output index

		cVal := existsRawCredit(ns, cKey)
		if cVal == nil {
			return fmt.Errorf("couldn't find a credit for unspent txo")
		}

		utxoAmt, err := fetchRawCreditAmount(cVal)
		if err != nil {
			return err
		}
		amt += utxoAmt

		return nil
	})

	return amt, err
}

// InsertTxScript is the exported version of insertTxScript.
func (s *Store) InsertTxScript(script []byte) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	err := scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		var err error
		err = s.insertTxScript(ns, script)
		return err
	})
	return err
}

// insertTxScript inserts a transaction script into the database.
func (s *Store) insertTxScript(ns walletdb.Bucket, script []byte) error {
	return putTxScript(ns, script)
}

// GetTxScript is the exported version of getTxScript.
func (s *Store) GetTxScript(hash []byte) ([]byte, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var script []byte
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		script = s.getTxScript(ns, hash)
		return nil
	})
	return script, err
}

// getTxScript fetches a transaction script from the database using
// the RIPEMD160 hash as a key.
func (s *Store) getTxScript(ns walletdb.Bucket, hash []byte) []byte {
	return existsTxScript(ns, hash)
}

// StoredTxScripts is the exported version of storedTxScripts.
func (s *Store) StoredTxScripts() ([][]byte, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var scripts [][]byte
	err := scopedView(s.namespace, func(ns walletdb.Bucket) error {
		localScripts, localErr := s.storedTxScripts(ns)
		scripts = localScripts
		return localErr
	})
	return scripts, err
}

// storedTxScripts returns a slice of byte slices containing all the transaction
// scripts currently stored in wallet.
func (s *Store) storedTxScripts(ns walletdb.Bucket) ([][]byte, error) {
	var scripts [][]byte
	err := ns.Bucket(bucketScripts).ForEach(func(k, v []byte) error {
		scripts = append(scripts, v)
		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return nil, err
		}
		str := "failed iterating scripts"
		return nil, storeError(ErrDatabase, str, err)
	}
	return scripts, err
}

// RepairInconsistencies attempts to repair the databases in the event that data
// has gone missing. This throws very loud errors to the user, because it should
// ideally never happen and indicates wallet corruption. It returns a list of
// UTXOs so wallet can further investigate whether or not they exist in daemon
// and, if they don't, can trigger their deletion.
//
func (s *Store) RepairInconsistencies() ([]*wire.OutPoint, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var utxos []*wire.OutPoint
	err := scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		ut, localErr := s.repairInconsistencies(ns)
		utxos = ut
		return localErr
	})
	return utxos, err
}

type dbCredit struct {
	op *wire.OutPoint
	bl *Block
}

func (s *Store) repairInconsistencies(ns walletdb.Bucket) ([]*wire.OutPoint,
	error) {
	var unspent []*wire.OutPoint
	var badUnspent []*wire.OutPoint
	var badCredit []*dbCredit

	// Unspent should map 1:1 with credits. If the credit can't be found, the utxo
	// should be deleted. If the credit can be found but the transaction or block
	// doesn't exist, the credit and unspent should be deleted.
	err := ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		op := new(wire.OutPoint)
		readCanonicalOutPoint(k, op)
		bl := new(Block)
		readUnspentBlock(v, bl)

		// Look up credit.
		creditKey := keyCredit(&op.Hash, op.Index, bl)
		creditValue := existsRawCredit(ns, creditKey)
		if creditValue == nil {
			badUnspent = append(badUnspent, op)
			return nil
		}

		// Look up transaction.
		txKey := keyTxRecord(&op.Hash, bl)
		txValue := existsRawTxRecord(ns, txKey)
		if txValue == nil {
			badUnspent = append(badUnspent, op)
			badCredit = append(badCredit, &dbCredit{op, bl})
		}

		// Look up block.
		_, blockValue := existsBlockRecord(ns, bl.Height)
		if blockValue == nil {
			badUnspent = append(badUnspent, op)
			badCredit = append(badCredit, &dbCredit{op, bl})
		}

		unspent = append(unspent, op)
		return nil
	})
	if err != nil {
		if _, ok := err.(Error); ok {
			return nil, err
		}
		str := "failed iterating unspent"
		return nil, storeError(ErrDatabase, str, err)
	}

	// Destroy everything that is causing issues.
	for _, bad := range badCredit {
		creditKey := keyCredit(&bad.op.Hash, bad.op.Index, bad.bl)
		err := deleteRawCredit(ns, creditKey)
		if err != nil {
			return nil, err
		}
	}
	for _, bad := range badUnspent {
		unspentKey := canonicalOutPoint(&bad.Hash, bad.Index)
		err := deleteRawUnspent(ns, unspentKey)
		if err != nil {
			return nil, err
		}
	}

	// This code doesn't handle inconsistencies in unmined credits. TODO.

	return unspent, err
}

// DeleteUnspent allows an external caller to delete unspent transaction outputs
// of its choosing, e.g. if those unspent outpoint transactions are found to not
// exist in the daemon.
func (s *Store) DeleteUnspent(utxos []*wire.OutPoint) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	err := scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		localErr := s.deleteUnspent(ns, utxos)
		return localErr
	})
	return err
}

func (s *Store) deleteUnspent(ns walletdb.Bucket, utxos []*wire.OutPoint) error {
	// Look up the credit and see if it exists; if it does, we want to
	// get rid of that too.
	for _, bad := range utxos {
		unspentKey := canonicalOutPoint(&bad.Hash, bad.Index)
		unspentValue := existsRawUnspent(ns, unspentKey)
		if unspentValue == nil {
			str := "failed to find unspent outpoint"
			return storeError(ErrDatabase, str, nil)
		}
		bl := new(Block)
		readUnspentBlock(unspentValue, bl)

		// We don't actually care if the credit deletion fails, it may or
		// may not be there.
		creditKey := keyCredit(&bad.Hash, bad.Index, bl)
		deleteRawCredit(ns, creditKey)

		// The unspent output should definitely be there, though.
		err := deleteRawUnspent(ns, unspentKey)
		if err != nil {
			return err
		}
	}

	return nil
}

// RepairMinedBalance allows an external caller to attempt to fix the mined
// balance with a full scan balance call.
func (s *Store) RepairMinedBalance(curHeight int32) error {
	if s.isClosed {
		str := "tx manager is closed"
		return storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	err := scopedUpdate(s.namespace, func(ns walletdb.Bucket) error {
		localErr := s.repairMinedBalance(ns, curHeight)
		return localErr
	})
	return err
}

func (s *Store) repairMinedBalance(ns walletdb.Bucket, curHeight int32) error {
	bal, err := s.balanceFullScan(ns, 1, curHeight)
	if err != nil {
		return err
	}

	return putMinedBalance(ns, bal)
}

// DatabaseContents is a struct of maps pointing to the current contents of the
// database.
type DatabaseContents struct {
	MinedBalance         dcrutil.Amount
	OneConfBalance       dcrutil.Amount
	OneConfCalcBalance   dcrutil.Amount
	BucketBlocks         map[string][]byte
	BucketTxRecords      map[string][]byte
	BucketCredits        map[string][]byte
	BucketUnspent        map[string][]byte
	BucketDebits         map[string][]byte
	BucketUnmined        map[string][]byte
	BucketUnminedCredits map[string][]byte
	BucketUnminedInputs  map[string][]byte
	BucketScripts        map[string][]byte
	BucketMultisig       map[string][]byte
	BucketMultisigUsp    map[string][]byte
}

// reverse copies a string into a byte slice and then reverses it.
func reverse(s string) (result string) {
	bs, _ := hex.DecodeString(s)
	for left, right := 0, len(bs)-1; left < right; left, right = left+1, right-1 {
		bs[left], bs[right] = bs[right], bs[left]
	}

	return hex.EncodeToString(bs)
}

// equalsMap compares two maps and notes any incongruencies in a string. It
// also returns whether or not the two maps were equal.
func equalsMap(m1 map[string][]byte, m2 map[string][]byte, mapName string) (bool,
	string) {
	var buffer bytes.Buffer

	if len(m1) != len(m2) {
		str := fmt.Sprintf("Map size inconsistency; map 1 is %v many items, "+
			"while map 2 is %v many items\n",
			len(m1), len(m2))
		buffer.WriteString(str)
	}

	for k1, v1 := range m1 {
		v2, exists := m2[k1]
		if !exists {
			str := fmt.Sprintf("%v inconsistency\n "+
				"Key %v exists in map 1 but not in map 2 "+
				"(little endian of key: %v, val contents: %x)\n",
				mapName,
				k1,
				reverse(k1),
				v1)
			buffer.WriteString(str)
			continue
		}

		if !bytes.Equal(v1, v2) {
			str := fmt.Sprintf("%v inconsistency\n "+
				"Value for key %v (LE: %v) is %x in map 1, but %x in map 2\n",
				mapName,
				k1, reverse(k1), v1, v2)
			buffer.WriteString(str)
		}
	}

	for k2, v2 := range m2 {
		_, exists := m1[k2]
		if !exists {
			str := fmt.Sprintf("%v inconsistency\n "+
				"Key %v exists in map 2 but not in map 1\n "+
				"(little endian of key: %v, val contents: %x)\n",
				mapName,
				k2,
				reverse(k2),
				v2)
			buffer.WriteString(str)
		}
	}

	bs := buffer.String()
	if bs == "" {
		return true, bs
	}

	return false, bs
}

// Equals compares two databases and returns a list of incongruencies between
// them.
func (d1 *DatabaseContents) Equals(d2 *DatabaseContents, skipUnmined bool) (bool,
	string) {
	var buffer bytes.Buffer
	if d1.MinedBalance != d2.MinedBalance {
		str := fmt.Sprintf("Mined balance incongruent; in the first element it "+
			"is %v, while in the second it is %v\n", d1.MinedBalance,
			d2.MinedBalance)
		buffer.WriteString(str)
	}

	if d1.OneConfBalance != d2.OneConfBalance {
		str := fmt.Sprintf("One conf spendable balance incongruent; in the "+
			"first element it is %v, while in the second it is %v\n",
			d1.OneConfBalance,
			d2.OneConfBalance)
		buffer.WriteString(str)
	}

	if d1.OneConfCalcBalance != d2.OneConfCalcBalance {
		str := fmt.Sprintf("One conf calculated spendable balance "+
			"incongruent; in the first element it is %v, while in the "+
			"second it is %v\n",
			d1.OneConfCalcBalance,
			d2.OneConfCalcBalance)
		buffer.WriteString(str)
	}

	is, str := equalsMap(d1.BucketBlocks, d2.BucketBlocks, "BucketBlocks")
	if !is {
		buffer.WriteString(str)
	}
	is, str = equalsMap(d1.BucketTxRecords, d2.BucketTxRecords, "BucketTxRecords")
	if !is {
		buffer.WriteString(str)
	}
	is, str = equalsMap(d1.BucketCredits, d2.BucketCredits, "BucketCredits")
	if !is {
		buffer.WriteString(str)
	}
	is, str = equalsMap(d1.BucketUnspent, d2.BucketUnspent, "BucketUnspent")
	if !is {
		buffer.WriteString(str)
	}
	is, str = equalsMap(d1.BucketDebits, d2.BucketDebits, "BucketDebits")
	if !is {
		buffer.WriteString(str)
	}

	if !skipUnmined {
		is, str = equalsMap(d1.BucketUnmined, d2.BucketUnmined, "BucketUnmined")
		if !is {
			buffer.WriteString(str)
		}
		is, str = equalsMap(d1.BucketUnminedCredits, d2.BucketUnminedCredits,
			"BucketUnminedCredits")
		if !is {
			buffer.WriteString(str)
		}
		is, str = equalsMap(d1.BucketUnminedInputs, d2.BucketUnminedInputs,
			"BucketUnminedInputs")
		if !is {
			buffer.WriteString(str)
		}
	}

	is, str = equalsMap(d1.BucketScripts, d2.BucketScripts, "BucketScripts")
	if !is {
		buffer.WriteString(str)
	}
	is, str = equalsMap(d1.BucketMultisig, d2.BucketMultisig, "BucketMultisig")
	if !is {
		buffer.WriteString(str)
	}
	is, str = equalsMap(d1.BucketMultisigUsp, d2.BucketMultisigUsp,
		"BucketMultisigUsp")
	if !is {
		buffer.WriteString(str)
	}

	bs := buffer.String()
	if bs == "" {
		return true, ""
	}

	return false, bs
}

// DatabaseDump is a testing function for wallet that exports the contents of
// all databases as a
func (s *Store) DatabaseDump(height int32,
	oldUnminedInputs map[string][]byte) (*DatabaseContents, error) {
	if s.isClosed {
		str := "tx manager is closed"
		return nil, storeError(ErrIsClosed, str, nil)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var dbDump *DatabaseContents
	var err error
	err = scopedView(s.namespace, func(ns walletdb.Bucket) error {
		var localErr error
		dbDump, localErr = s.generateDatabaseDump(ns, height, oldUnminedInputs)
		return localErr
	})
	return dbDump, err
}

// storedTxScripts returns a slice of byte slices containing all the transaction
// scripts currently stored in wallet.
func (s *Store) generateDatabaseDump(ns walletdb.Bucket,
	height int32, oldUnminedInputs map[string][]byte) (*DatabaseContents, error) {

	dbDump := new(DatabaseContents)
	dbDump.BucketBlocks = make(map[string][]byte)
	dbDump.BucketTxRecords = make(map[string][]byte)
	dbDump.BucketCredits = make(map[string][]byte)
	dbDump.BucketUnspent = make(map[string][]byte)
	dbDump.BucketDebits = make(map[string][]byte)
	dbDump.BucketUnmined = make(map[string][]byte)
	dbDump.BucketUnminedCredits = make(map[string][]byte)
	dbDump.BucketUnminedInputs = make(map[string][]byte)
	dbDump.BucketScripts = make(map[string][]byte)
	dbDump.BucketMultisig = make(map[string][]byte)
	dbDump.BucketMultisigUsp = make(map[string][]byte)

	bal, err := fetchMinedBalance(ns)
	if err != nil {
		return nil, err
	}
	dbDump.MinedBalance = bal

	if oldUnminedInputs == nil {
		dbDump.OneConfBalance, err = s.balanceSpendable(ns, 1, height)
		if err != nil {
			return nil, err
		}
	} else {
		dbDump.OneConfBalance, err = s.balanceSpendableSimulated(ns, 1, height,
			oldUnminedInputs)
		if err != nil {
			return nil, err
		}
	}

	if oldUnminedInputs == nil {
		dbDump.OneConfCalcBalance, err = s.balanceFullScan(ns, 1, height)
		if err != nil {
			return nil, err
		}
	} else {
		dbDump.OneConfCalcBalance, err = s.balanceFullScanSimulated(ns, 1, height,
			oldUnminedInputs)
		if err != nil {
			return nil, err
		}
	}

	ns.Bucket(bucketBlocks).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		vCopy := make([]byte, len(v), len(v))
		copy(vCopy, v)
		dbDump.BucketBlocks[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketTxRecords).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketTxRecords[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketCredits).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketCredits[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketUnspent).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketUnspent[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketDebits).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketDebits[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketUnmined).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketUnmined[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketUnminedCredits).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketUnminedCredits[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketUnminedInputs).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketUnminedInputs[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketScripts).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketScripts[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketMultisig).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketMultisig[kStr] = append([]byte(nil), v...)
		return nil
	})
	ns.Bucket(bucketMultisigUsp).ForEach(func(k, v []byte) error {
		kStr := hex.EncodeToString(k)
		dbDump.BucketMultisigUsp[kStr] = append([]byte(nil), v...)
		return nil
	})

	return dbDump, nil
}
