// Copyright 2019 The gonex Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package dccs implements the proof-of-foundation consensus engine.
package dccs

import (
	"bytes"
	"errors"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
	"golang.org/x/crypto/sha3"
)

type SealingQueue struct {
	hash       common.Hash                 // hash of the header
	sealer     common.Address              // sealer address of the header
	seed       []byte                      // random seed
	active     map[common.Address]struct{} // active sealers for the next block
	recent     map[common.Address]struct{} // recently signed sealers
	sorted     []common.Address            // sorted queue ([active]-[recent]+sealer)
	digest     common.Hash                 // hash(sort(active))
	sortedOnce sync.Once
	digestOnce sync.Once
}

func addressesHash(adrs []common.Address) common.Hash {
	hasher := sha3.NewLegacyKeccak256()
	for _, adr := range adrs {
		hasher.Write(adr[:])
	}
	return common.BytesToHash(hasher.Sum(nil))
}

func (q *SealingQueue) sealersDigest() common.Hash {
	q.digestOnce.Do(func() {
		active := make([]common.Address, 0, len(q.active))
		for adr := range q.active {
			active = append(active, adr)
		}
		sort.Sort(signersAscending(active))
		q.digest = addressesHash(active)
	})
	return q.digest
}

// return the common least ratio, and whether the continuity is broken
func (q *SealingQueue) commonRatio(r *SealingQueue) (*big.Rat, bool) {
	qLen := len(q.active)
	rLen := len(r.active)
	var smaller, larger map[common.Address]struct{}
	if qLen < rLen {
		smaller, larger = q.active, r.active
	} else {
		smaller, larger = r.active, q.active
	}
	common := 0
	for a := range smaller {
		if _, ok := larger[a]; ok {
			common++
		}
	}
	ratio := big.NewRat(int64(common), int64(len(larger)))
	// common should not be less that super majority of both queues
	broken := common*3 < qLen*2 || common*3 < rLen*2
	return ratio, broken
}

// sealerShuffling implements the sort interface to allow sorting a list of addresses
type seedShuffle struct {
	seed  []byte
	queue []common.Address
}

func (s seedShuffle) Len() int      { return len(s.queue) }
func (s seedShuffle) Swap(i, j int) { s.queue[i], s.queue[j] = s.queue[j], s.queue[i] }
func (s seedShuffle) Less(i, j int) bool {
	// TODO cache these hashes
	sha := func(address common.Address, seed []byte) []byte {
		hasher := sha3.NewLegacyKeccak256()
		hasher.Write(address[:])
		hasher.Write(seed)
		return hasher.Sum(nil)
	}
	si := sha(s.queue[i], s.seed)
	sj := sha(s.queue[j], s.seed)
	return bytes.Compare(si, sj) < 0
}

// include the last sealer in for positioning even when the last sealer is just left/leaked
func (q *SealingQueue) sortedQueue() []common.Address {
	q.sortedOnce.Do(func() {
		size := len(q.active) - len(q.recent) + 1
		s := seedShuffle{
			seed:  q.seed,
			queue: make([]common.Address, 1, size),
		}
		s.queue[0] = q.sealer
		for adr := range q.active {
			_, recentlySigned := q.recent[adr]
			if !recentlySigned {
				s.queue = append(s.queue, adr)
				// TODO: pre-calculate the shuffling hash here
			}
		}
		sort.Sort(s)
		q.sorted = s.queue
	})
	return q.sorted
}

func (q *SealingQueue) isRecentlySigned(address common.Address) bool {
	_, recentlySigned := q.recent[address]
	return recentlySigned
}

func (q *SealingQueue) isActive(address common.Address) bool {
	_, active := q.active[address]
	return active
}

func (q *SealingQueue) offset(signer common.Address,
	getHeaderByHash func(common.Hash) *types.Header,
	sigCache *lru.ARCCache) (int, error) {

	activeLen := len(q.active)

	if activeLen-len(q.recent) <= 1 {
		// no competition
		return 0, nil
	}

	if signer == q.sealer {
		return activeLen, errRecentlySigned
	}

	queue := q.sortedQueue()

	signerPosition := func(signer common.Address) (int, bool) {
		for i, sig := range queue {
			if sig == signer {
				return i, true
			}
		}
		return -1, false
	}

	pos, ok := signerPosition(signer)
	if !ok {
		// unable to find the signer position
		return activeLen, errUnauthorizedSigner
	}

	prevPos, ok := signerPosition(q.sealer)
	if !ok {
		// unable to find the previous signer position: should never happen
		return activeLen, errUnknownPreviousSealer
	}

	offset := pos - prevPos - 1
	if offset < 0 {
		offset += len(queue)
	}

	log.Trace("SealingQueue.offset",
		"offset", offset,
		"signer position", pos,
		"previous signer position", prevPos,
		"len(queue)", len(queue),
		"len(active)", len(q.active),
		"len(recent)", len(q.recent))

	return offset, nil
}

func (q *SealingQueue) difficulty(address common.Address,
	getHeaderByHash func(common.Hash) *types.Header,
	sigCache *lru.ARCCache) uint64 {

	offset, err := q.offset(address, getHeaderByHash, sigCache)
	if err != nil {
		return 0
	}

	n := len(q.active)

	return uint64(n - offset)
}

// recents len is MIN(lastActiveLen,activeLen)/2
func (c *Context) getSealingQueue(parentHash common.Hash) (*SealingQueue, error) {
	if q, ok := c.engine.sealingQueueCache.Get(parentHash); ok {
		// in-memory SealingQueue found
		queue := q.(*SealingQueue)
		return queue, nil
	}
	log.Trace("getSealingQueue", "parentHash", parentHash)
	parent := c.getHeaderByHash(parentHash)
	if parent == nil {
		return nil, errUnknownPreviousSealer
	}
	sealer, err := c.ecrecover(parent)
	if err != nil {
		return nil, err
	}
	randomData, err := c.getChainRandomSeed(parent)
	if err != nil {
		return nil, err
	}
	queue := SealingQueue{
		hash:   parentHash,
		sealer: sealer,
		seed:   randomData,
		active: map[common.Address]struct{}{},
		recent: map[common.Address]struct{}{},
	}

	number := parent.Number.Uint64()

	// temporary queue for recents
	var recents []common.Address

	addRecent := func(sealer common.Address) {
		if _, exists := queue.recent[sealer]; !exists {
			queue.recent[sealer] = struct{}{}
			recents = append(recents, sealer)
		}
	}
	addActive := func(sealer common.Address) {
		if _, exists := queue.active[sealer]; !exists {
			queue.active[sealer] = struct{}{}
		}
	}
	remActive := func(sealer common.Address) {
		if _, exists := queue.active[sealer]; exists {
			delete(queue.active, sealer)
		}
	}

	var maxDiff uint64

	// scan backward atmost LeakDurations blocks from number
	for i := uint64(0); i < c.engine.config.LeakDuration; i++ {
		if number <= i {
			break // stop at the genesis block
		}

		// OPTIMIZATION: for non-leakage case
		// correct if there's at least one in-turn block in the last 16 blocks
		const minBlockToScan = 16
		if i > minBlockToScan && len(queue.active) >= int(maxDiff) {
			break // all active sealers has probably been collected
		}
		// END OF OPTIMIZATION

		// TODO: optimization for leakage case

		n := number - i
		header := c.getHeaderByNumber(n)
		if header == nil {
			log.Error("getSealingQueue: getAvailableHeader returns nil", "n", n, "len(parents)", len(c.parents))
			return nil, errUnknownBlock
		}
		sealer, err := c.ecrecover(header)
		if err != nil {
			return nil, err
		}

		addActive(sealer)

		// use the difficulty for total number of recently active sealer count
		if header.Difficulty.Uint64() > maxDiff {
			maxDiff = header.Difficulty.Uint64()
		}
		// somewhat probabilistically optimized, fairly safe nonetheless
		if i < minBlockToScan || len(recents) < int(maxDiff)/2 {
			addRecent(sealer)
		}
	}

	apps, err := c.crawlSealerApplications(parent)
	if err != nil {
		return nil, err
	}
	for _, app := range apps {
		if app.isJoined() {
			log.Trace("Sealer application", "address", app.sealer, "joined", app.isJoined())
			addActive(app.sealer)
		} else {
			log.Error("Sealer application", "address", app.sealer, "joined", app.isJoined())
			remActive(app.sealer)
		}
	}

	// truncate the extra recents
	if len(queue.active)/2 < len(recents) {
		for i := len(queue.active) / 2; i < len(recents); i++ {
			delete(queue.recent, recents[i])
		}
	}

	// Store found snapshot into mem-cache
	c.engine.sealingQueueCache.Add(queue.hash, &queue)
	return &queue, nil
}

// crawl back the sealer applications skip-list
func (c *Context) crawlSealerApplications(header *types.Header) ([]SealerApplication, error) {
	number := header.Number.Uint64()
	apps := []SealerApplication{}
	for header := c.getHeaderByHash(header.MixDigest); header != nil; header = c.getHeaderByHash(header.MixDigest) {
		log.Trace("crawling", "appNumber", header.Number, "appNumber.Hash", header.Hash(), "cross-link", header.MixDigest)
		if (header.MixDigest == common.Hash{}) {
			// reach the CoLoa hardfork (new genesis)
			break
		}
		appConfirmedNumber := header.Number.Uint64() + c.engine.config.ApplicationConfirmation
		// condition: appConfirmedNumber in (number-LeakDuration;number]
		if appConfirmedNumber+c.engine.config.LeakDuration <= number {
			// any applications from this would be too old
			break
		}
		if appConfirmedNumber > number {
			// not enough confirmation
			continue
		}
		if len(header.Extra) <= extraVanity+extraSeal {
			log.Error("no sealer application data in header extra", "app number", header.Number, "number", number)
			return nil, errors.New("no sealer application data in header extra")
		}
		link, _, err := anchorDataFrom(header.Extra[extraVanity : len(header.Extra)-extraSeal])
		if err != nil {
			return nil, err
		}
		if len(link.applications) > 0 {
			apps = append(link.applications, apps...)
		}
	}
	return apps, nil
}
