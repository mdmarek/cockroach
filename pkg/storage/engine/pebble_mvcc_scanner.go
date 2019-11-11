// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package engine

import (
	"bytes"
	"encoding/binary"
	"sort"
	"sync"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble"
)

const (
	maxItersBeforeSeek = 10
)

// Struct to store MVCCScan / MVCCGet in the same binary format as that
// expected by MVCCScanDecodeKeyValue.
type pebbleResults struct {
	count int64
	repr  []byte
	bufs  [][]byte
}

func (p *pebbleResults) clear() {
	*p = pebbleResults{}
}

// The repr that MVCCScan / MVCCGet expects to provide as output goes:
// <valueLen:Uint32><keyLen:Uint32><Key><Value>
// This function adds to repr in that format.
func (p *pebbleResults) put(key []byte, value []byte) {
	// Key value lengths take up 8 bytes (2 x Uint32).
	const kvLenSize = 8
	const minSize = 16
	const maxSize = 128 << 20 // 128 MB

	// We maintain a list of buffers, always encoding into the last one (a.k.a.
	// pebbleResults.repr). The size of the buffers is exponentially increasing,
	// capped at maxSize.
	lenToAdd := kvLenSize + len(key) + len(value)
	if len(p.repr)+lenToAdd > cap(p.repr) {
		newSize := 2 * cap(p.repr)
		if newSize == 0 {
			newSize = minSize
		}
		for newSize < lenToAdd && newSize < maxSize {
			newSize *= 2
		}
		if len(p.repr) > 0 {
			p.bufs = append(p.bufs, p.repr)
		}
		p.repr = nonZeroingMakeByteSlice(newSize)[:0]
	}

	startIdx := len(p.repr)
	p.repr = p.repr[:startIdx+lenToAdd]
	binary.LittleEndian.PutUint32(p.repr[startIdx:], uint32(len(value)))
	binary.LittleEndian.PutUint32(p.repr[startIdx+4:], uint32(len(key)))
	copy(p.repr[startIdx+kvLenSize:], key)
	copy(p.repr[startIdx+kvLenSize+len(key):], value)
	p.count++
}

func (p *pebbleResults) finish() [][]byte {
	if len(p.repr) > 0 {
		p.bufs = append(p.bufs, p.repr)
		p.repr = nil
	}
	return p.bufs
}

// Go port of mvccScanner in libroach/mvcc.h. Stores all variables relating to
// one MVCCGet / MVCCScan call.
type pebbleMVCCScanner struct {
	parent  *pebble.Iterator
	reverse bool
	peeked  bool
	// Iteration bounds. Does not contain MVCC timestamp.
	start, end roachpb.Key
	// Timestamp with which MVCCScan/MVCCGet was called.
	ts hlc.Timestamp
	// Max number of keys to return.
	maxKeys int64
	// Transaction epoch and sequence number.
	txn         *roachpb.Transaction
	txnEpoch    enginepb.TxnEpoch
	txnSequence enginepb.TxnSeq
	// Metadata object for unmarshalling intents.
	meta enginepb.MVCCMetadata
	// Bools copied over from MVCC{Scan,Get}Options. See the comment on the
	// package level MVCCScan for what these mean.
	inconsistent, tombstones bool
	checkUncertainty         bool
	keyBuf                   []byte
	savedBuf                 []byte
	// cur* variables store the "current" record we're pointing to. Updated in
	// updateCurrent. Note that curRawKey = the full encoded MVCC key, while
	// curKey = the user-key part of curRawKey (i.e. excluding the timestamp).
	curRawKey, curKey, curValue []byte
	curTS                       hlc.Timestamp
	results                     pebbleResults
	intents                     pebble.Batch
	// Stores any error returned. If non-nil, iteration short circuits.
	err error
	// Number of iterations to try before we do a Seek/SeekReverse. Stays within
	// [1, maxItersBeforeSeek] and defaults to maxItersBeforeSeek/2 .
	itersBeforeSeek int
}

// Pool for allocating pebble MVCC Scanners.
var pebbleMVCCScannerPool = sync.Pool{
	New: func() interface{} {
		return &pebbleMVCCScanner{}
	},
}

// init sets bounds on the underlying pebble iterator, and initializes other
// fields not set by the calling method.
func (p *pebbleMVCCScanner) init(txn *roachpb.Transaction) {
	p.itersBeforeSeek = maxItersBeforeSeek / 2

	if txn != nil {
		p.txn = txn
		p.txnEpoch = txn.Epoch
		p.txnSequence = txn.Sequence
		p.checkUncertainty = p.ts.Less(txn.MaxTimestamp)
	}
}

// get iterates exactly once and adds one KV to the result set.
func (p *pebbleMVCCScanner) get() {
	p.keyBuf = EncodeKeyToBuf(p.keyBuf[:0], MVCCKey{Key: p.start})
	valid := p.parent.SeekPrefixGE(p.keyBuf)
	if !p.updateCurrent(valid) {
		return
	}
	p.getAndAdvance()
}

// scan iterates until maxKeys records are in results, or the underlying
// iterator is exhausted, or an error is encountered.
func (p *pebbleMVCCScanner) scan() (*roachpb.Span, error) {
	if p.reverse {
		p.keyBuf = EncodeKeyToBuf(p.keyBuf[:0], MVCCKey{Key: p.end})
		if !p.iterSeekReverse(p.keyBuf) {
			return nil, p.err
		}
	} else {
		p.keyBuf = EncodeKeyToBuf(p.keyBuf[:0], MVCCKey{Key: p.start})
		if !p.iterSeek(p.keyBuf) {
			return nil, p.err
		}
	}

	for p.getAndAdvance() {
	}

	var resume *roachpb.Span
	if p.results.count == p.maxKeys && p.advanceKey() {
		if p.reverse {
			// curKey was not added to results, so it needs to be included in the
			// resume span.
			resume = &roachpb.Span{
				Key:    p.start,
				EndKey: roachpb.Key(p.curKey).Next(),
			}
		} else {
			resume = &roachpb.Span{
				Key:    p.curKey,
				EndKey: p.end,
			}
		}
	}
	return resume, p.err
}

// Increments itersBeforeSeek while ensuring it stays <= maxItersBeforeSeek
func (p *pebbleMVCCScanner) incrementItersBeforeSeek() {
	p.itersBeforeSeek++
	if p.itersBeforeSeek > maxItersBeforeSeek {
		p.itersBeforeSeek = maxItersBeforeSeek
	}
}

// Decrements itersBeforeSeek while ensuring it stays positive.
func (p *pebbleMVCCScanner) decrementItersBeforeSeek() {
	p.itersBeforeSeek--
	if p.itersBeforeSeek < 1 {
		p.itersBeforeSeek = 1
	}
}

// Try to read from the current value's intent history. Assumes p.meta has been
// unmarshalled already. Returns true if a value was read and added to the
// result set.
func (p *pebbleMVCCScanner) getFromIntentHistory() bool {
	intentHistory := p.meta.IntentHistory
	// upIdx is the index of the first intent in intentHistory with a sequence
	// number greater than our transaction's sequence number. Subtract 1 from it
	// to get the index of the intent with the highest sequence number that is
	// still less than or equal to p.txnSeq.
	upIdx := sort.Search(len(intentHistory), func(i int) bool {
		return intentHistory[i].Sequence > p.txnSequence
	})
	if upIdx == 0 {
		// It is possible that no intent exists such that the sequence is less
		// than the read sequence. In this case, we cannot read a value from the
		// intent history.
		return false
	}
	intent := p.meta.IntentHistory[upIdx-1]
	if len(intent.Value) > 0 || p.tombstones {
		p.results.put(p.curRawKey, intent.Value)
	}
	return true
}

// Returns an uncertainty error with the specified timestamp and p.txn.
func (p *pebbleMVCCScanner) uncertaintyError(ts hlc.Timestamp) bool {
	p.err = roachpb.NewReadWithinUncertaintyIntervalError(p.ts, ts, p.txn)
	p.results.clear()
	p.intents.Reset()
	return false
}

// Emit a tuple and return true if we have reason to believe iteration can
// continue.
func (p *pebbleMVCCScanner) getAndAdvance() bool {
	mvccKey := MVCCKey{p.curKey, p.curTS}
	if mvccKey.IsValue() {
		if !p.ts.Less(p.curTS) {
			// 1. Fast path: there is no intent and our read timestamp is newer than
			// the most recent version's timestamp.
			return p.addAndAdvance(p.curValue)
		}

		if p.checkUncertainty {
			// 2. Our txn's read timestamp is less than the max timestamp
			// seen by the txn. We need to check for clock uncertainty
			// errors.
			if !p.txn.MaxTimestamp.Less(p.curTS) {
				return p.uncertaintyError(p.curTS)
			}

			return p.seekVersion(p.txn.MaxTimestamp, true)
		}

		// 3. Our txn's read timestamp is greater than or equal to the
		// max timestamp seen by the txn so clock uncertainty checks are
		// unnecessary. We need to seek to the desired version of the
		// value (i.e. one with a timestamp earlier than our read
		// timestamp).
		return p.seekVersion(p.ts, false)
	}

	if len(p.curValue) == 0 {
		p.err = errors.Errorf("zero-length mvcc metadata")
		return false
	}
	err := protoutil.Unmarshal(p.curValue, &p.meta)
	if err != nil {
		p.err = errors.Errorf("unable to decode MVCCMetadata: %s", err)
		return false
	}
	if len(p.meta.RawBytes) != 0 {
		// 4. Emit immediately if the value is inline.
		return p.addAndAdvance(p.meta.RawBytes)
	}

	if p.meta.Txn == nil {
		p.err = errors.Errorf("intent without transaction")
		return false
	}
	metaTS := hlc.Timestamp(p.meta.Timestamp)

	// metaTS is the timestamp of an intent value, which we may or may
	// not end up ignoring, depending on factors codified below. If we do ignore
	// the intent then we want to read at a lower timestamp that's strictly
	// below the intent timestamp (to skip the intent), but also does not exceed
	// our read timestamp (to avoid erroneously picking up future committed
	// values); this timestamp is prevTS.
	prevTS := p.ts
	if !p.ts.Less(metaTS) {
		prevTS = metaTS.Prev()
	}

	ownIntent := p.txn != nil && p.meta.Txn.ID.Equal(p.txn.ID)
	maxVisibleTS := p.ts
	if p.checkUncertainty {
		maxVisibleTS = p.txn.MaxTimestamp
	}

	if maxVisibleTS.Less(metaTS) && !ownIntent {
		// 5. The key contains an intent, but we're reading before the
		// intent. Seek to the desired version. Note that if we own the
		// intent (i.e. we're reading transactionally) we want to read
		// the intent regardless of our read timestamp and fall into
		// case 8 below.
		return p.seekVersion(p.ts, false)
	}

	if p.inconsistent {
		// 6. The key contains an intent and we're doing an inconsistent
		// read at a timestamp newer than the intent. We ignore the
		// intent by insisting that the timestamp we're reading at is a
		// historical timestamp < the intent timestamp. However, we
		// return the intent separately; the caller may want to resolve
		// it.
		if p.results.count == p.maxKeys {
			// We've already retrieved the desired number of keys and now
			// we're adding the resume key. We don't want to add the
			// intent here as the intents should only correspond to KVs
			// that lie before the resume key.
			return false
		}
		p.err = p.intents.Set(p.curRawKey, p.curValue, nil)
		if p.err != nil {
			return false
		}

		return p.seekVersion(prevTS, false)
	}

	if !ownIntent {
		// 7. The key contains an intent which was not written by our
		// transaction and our read timestamp is newer than that of the
		// intent. Note that this will trigger an error on the Go
		// side. We continue scanning so that we can return all of the
		// intents in the scan range.
		p.err = p.intents.Set(p.curRawKey, p.curValue, nil)
		if p.err != nil {
			return false
		}
		return p.advanceKey()
	}

	if p.txnEpoch == p.meta.Txn.Epoch {
		if p.txnSequence >= p.meta.Txn.Sequence {
			// 8. We're reading our own txn's intent at an equal or higher sequence.
			// Note that we read at the intent timestamp, not at our read timestamp
			// as the intent timestamp may have been pushed forward by another
			// transaction. Txn's always need to read their own writes.
			return p.seekVersion(metaTS, false)
		}

		// 9. We're reading our own txn's intent at a lower sequence than is
		// currently present in the intent. This means the intent we're seeing
		// was written at a higher sequence than the read and that there may or
		// may not be earlier versions of the intent (with lower sequence
		// numbers) that we should read. If there exists a value in the intent
		// history that has a sequence number equal to or less than the read
		// sequence, read that value.
		if p.getFromIntentHistory() {
			if p.results.count == p.maxKeys {
				return false
			}
			return p.advanceKey()
		}
		// 10. If no value in the intent history has a sequence number equal to
		// or less than the read, we must ignore the intents laid down by the
		// transaction all together. We ignore the intent by insisting that the
		// timestamp we're reading at is a historical timestamp < the intent
		// timestamp.
		return p.seekVersion(prevTS, false)
	}

	if p.txnEpoch < p.meta.Txn.Epoch {
		// 11. We're reading our own txn's intent but the current txn has
		// an earlier epoch than the intent. Return an error so that the
		// earlier incarnation of our transaction aborts (presumably
		// this is some operation that was retried).
		p.err = errors.Errorf("failed to read with epoch %d due to a write intent with epoch %d",
			p.txnEpoch, p.meta.Txn.Epoch)
		return false
	}

	// 12. We're reading our own txn's intent but the current txn has a
	// later epoch than the intent. This can happen if the txn was
	// restarted and an earlier iteration wrote the value we're now
	// reading. In this case, we ignore the intent and read the
	// previous value as if the transaction were starting fresh.
	return p.seekVersion(prevTS, false)
}

// Advances to the next user key.
func (p *pebbleMVCCScanner) nextKey() bool {
	p.keyBuf = append(p.keyBuf[:0], p.curKey...)

	for i := 0; i < p.itersBeforeSeek; i++ {
		if !p.iterNext() {
			return false
		}
		if !bytes.Equal(p.curKey, p.keyBuf) {
			p.incrementItersBeforeSeek()
			return true
		}
	}

	p.decrementItersBeforeSeek()
	// We're pointed at a different version of the same key. Fall back to seeking
	// to the next key. We append 2 NULs to account for the "next-key" and a
	// trailing zero timestamp.
	p.keyBuf = append(p.keyBuf, 0, 0)
	return p.iterSeek(p.keyBuf)
}

// backwardLatestVersion backs up the iterator to the latest version for the
// specified key. The parameter i is used to maintain iteration count between
// the loop here and the caller (usually prevKey). Returns false if the
// iterator was exhausted. Assumes that the iterator is currently positioned at
// the oldest version of key.
func (p *pebbleMVCCScanner) backwardLatestVersion(key []byte, i int) bool {
	p.keyBuf = append(p.keyBuf[:0], key...)

	for ; i < p.itersBeforeSeek; i++ {
		peekedKey, ok := p.iterPeekPrev()
		if !ok {
			// No previous entry exists, so we're at the latest version of key.
			return true
		}
		if !bytes.Equal(peekedKey, p.keyBuf) {
			p.incrementItersBeforeSeek()
			return true
		}
		if !p.iterPrev() {
			return false
		}
	}

	p.decrementItersBeforeSeek()
	p.keyBuf = append(p.keyBuf, 0)
	return p.iterSeek(p.keyBuf)
}

// Advance to the newest version of the user key preceding the specified
// key. Assumes that the iterator is currently positioned at key or 1 record
// after key.
func (p *pebbleMVCCScanner) prevKey(key []byte) bool {
	p.keyBuf = append(p.keyBuf[:0], key...)

	for i := 0; i < p.itersBeforeSeek; i++ {
		peekedKey, ok := p.iterPeekPrev()
		if !ok {
			return false
		}
		if !bytes.Equal(peekedKey, p.keyBuf) {
			return p.backwardLatestVersion(peekedKey, i+1)
		}
		if !p.iterPrev() {
			return false
		}
	}

	p.decrementItersBeforeSeek()
	p.keyBuf = append(p.keyBuf, 0)
	return p.iterSeekReverse(p.keyBuf)
}

// Advance to the next key in the iterator's direction.
func (p *pebbleMVCCScanner) advanceKey() bool {
	if p.reverse {
		return p.prevKey(p.curKey)
	}
	return p.nextKey()
}

// advanceKeyAtEnd advances to the next key when the iterator's end has been
// reached.
func (p *pebbleMVCCScanner) advanceKeyAtEnd() bool {
	if p.reverse {
		// Iterating to the next key might have caused the iterator to reach the
		// end of the key space. If that happens, back up to the very last key.
		p.peeked = false
		valid := p.parent.Last()
		if !p.updateCurrent(valid) {
			return false
		}
		return p.advanceKey()
	}
	// We've reached the end of the iterator and there is nothing left to do.
	return false
}

// advanceKeyAtNewKey advances to the key after the specified key, assuming we
// have just reached the specified key.
func (p *pebbleMVCCScanner) advanceKeyAtNewKey(key []byte) bool {
	if p.reverse {
		// We've advanced to the next key but need to move back to the previous
		// key.
		return p.prevKey(key)
	}
	// We're already at the new key so there is nothing to do.
	return true
}

// Adds the specified value to the result set, excluding tombstones unless
// p.tombstones is true. Advances to the next key unless we've reached the max
// results limit.
func (p *pebbleMVCCScanner) addAndAdvance(val []byte) bool {
	// Don't include deleted versions len(val) == 0, unless we've been instructed
	// to include tombstones in the results.
	if len(val) > 0 || p.tombstones {
		p.results.put(p.curRawKey, val)
		if p.results.count == p.maxKeys {
			return false
		}
	}
	return p.advanceKey()
}

// Seeks to the latest revision of the current key that's still less than or
// equal to the specified timestamp, adds it to the result set, then moves onto
// the next user key.
func (p *pebbleMVCCScanner) seekVersion(ts hlc.Timestamp, uncertaintyCheck bool) bool {
	p.keyBuf = EncodeKeyToBuf(p.keyBuf[:0], MVCCKey{Key: p.curKey, Timestamp: ts})
	origKey := p.keyBuf[:len(p.curKey)]

	for i := 0; i < p.itersBeforeSeek; i++ {
		if !p.iterNext() {
			return p.advanceKeyAtEnd()
		}
		if !bytes.Equal(p.curKey, origKey) {
			p.incrementItersBeforeSeek()
			return p.advanceKeyAtNewKey(origKey)
		}
		if !ts.Less(p.curTS) {
			p.incrementItersBeforeSeek()
			if uncertaintyCheck && p.ts.Less(p.curTS) {
				return p.uncertaintyError(p.curTS)
			}
			return p.addAndAdvance(p.curValue)
		}
	}

	p.decrementItersBeforeSeek()
	if !p.iterSeek(p.keyBuf) {
		return p.advanceKeyAtEnd()
	}
	if !bytes.Equal(p.curKey, origKey) {
		return p.advanceKeyAtNewKey(origKey)
	}
	if !ts.Less(p.curTS) {
		if uncertaintyCheck && p.ts.Less(p.curTS) {
			return p.uncertaintyError(p.curTS)
		}
		return p.addAndAdvance(p.curValue)
	}
	return p.advanceKey()
}

// Updates cur{RawKey, Key, TS} to match record the iterator is pointing to.
func (p *pebbleMVCCScanner) updateCurrent(valid bool) bool {
	if !valid {
		return false
	}

	p.curRawKey = p.parent.Key()
	p.curValue = p.parent.Value()
	p.curKey, p.curTS, p.err = enginepb.DecodeKey(p.curRawKey)
	return p.err == nil
}

// seek seeks to the latest revision of the specified key (or a greater key).
func (p *pebbleMVCCScanner) iterSeek(key []byte) bool {
	p.clearPeeked()
	valid := p.parent.SeekGE(key)
	return p.updateCurrent(valid)
}

// seekReverse seeks to the latest revision of the key before the specified key.
func (p *pebbleMVCCScanner) iterSeekReverse(key []byte) bool {
	p.clearPeeked()

	valid := p.parent.SeekLT(key)
	if !p.updateCurrent(valid) {
		// We have seeked to before the start key. Return.
		return false
	}
	if p.curTS == (hlc.Timestamp{}) {
		// We landed on an intent or inline value.
		return true
	}

	// We landed on a versioned value, we need to back up to find the
	// latest version.
	return p.backwardLatestVersion(p.curKey, 0)
}

// Advance to the next MVCC key.
func (p *pebbleMVCCScanner) iterNext() bool {
	if p.reverse && p.peeked {
		// If we have peeked at the previous entry, we need to advance the iterator
		// twice.
		p.peeked = false
		if !p.parent.Valid() {
			// We were peeked off the beginning of iteration. Seek to the first
			// entry, and then advance one step.
			if !p.parent.First() {
				return false
			}
			return p.updateCurrent(p.parent.Next())
		}
		if !p.parent.Next() {
			return false
		}
	}
	valid := p.parent.Next()
	return p.updateCurrent(valid)
}

// Advance to the previous MVCC Key.
func (p *pebbleMVCCScanner) iterPrev() bool {
	if p.peeked {
		p.peeked = false
		return p.updateCurrent(p.parent.Valid())
	}
	valid := p.parent.Prev()
	return p.updateCurrent(valid)
}

// Peek the previous key and store the result in peekedKey. Note that this
// moves the iterator backward, while leaving p.cur{key,value,rawKey} untouched
// and therefore out of sync. iterPrev and iterNext take this into account.
func (p *pebbleMVCCScanner) iterPeekPrev() ([]byte, bool) {
	if !p.peeked {
		p.peeked = true
		// We need to save a copy of the current iterator key and value and adjust
		// curRawKey, curKey and curValue to point to this saved data. We use a
		// single buffer for this purpose: savedBuf.
		p.savedBuf = append(p.savedBuf[:0], p.curRawKey...)
		p.savedBuf = append(p.savedBuf, p.curValue...)
		p.curRawKey = p.savedBuf[:len(p.curRawKey)]
		p.curValue = p.savedBuf[len(p.curRawKey):]
		var ok bool
		p.curKey, _, ok = enginepb.SplitMVCCKey(p.curRawKey)
		if !ok {
			p.err = errors.Errorf("invalid encoded mvcc key: %x", p.curRawKey)
			return nil, false
		}

		// With the current iterator state saved we can move the iterator to the
		// previous entry.
		if !p.parent.Prev() {
			// The iterator is now invalid, but note that this case is handled in
			// both iterNext and iterPrev. In the former case, we'll position the
			// iterator at the first entry, and in the latter iteration will be done.
			return nil, false
		}
	} else if !p.parent.Valid() {
		return nil, false
	}

	peekedKey, _, ok := enginepb.SplitMVCCKey(p.parent.Key())
	if !ok {
		p.err = errors.Errorf("invalid encoded mvcc key: %x", p.parent.Key())
		return nil, false
	}
	return peekedKey, true
}

// Clear the peeked flag. Call this before any iterator operations.
func (p *pebbleMVCCScanner) clearPeeked() {
	if p.reverse {
		p.peeked = false
	}
}
