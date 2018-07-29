package blockchain

import (
	"github.com/hashicorp/golang-lru"
	"sync/atomic"

	"github.com/kardiachain/go-kardia/configs"
	"github.com/kardiachain/go-kardia/lib/common"

	"github.com/kardiachain/go-kardia/blockchain/rawdb"
	"github.com/kardiachain/go-kardia/lib/log"
	kaidb "github.com/kardiachain/go-kardia/storage"
	"github.com/kardiachain/go-kardia/types"
)

const (
	headerCacheLimit = 512
	heightCacheLimit = 2048
)

//TODO(huny@): Add detailed description
type HeaderChain struct {
	config *configs.ChainConfig

	chainDb kaidb.Database

	genesisHeader *types.Header

	currentHeader     atomic.Value // Current head of the header chain (may be above the block chain!)
	currentHeaderHash common.Hash  // Hash of the current head of the header chain (prevent recomputing all the time)

	headerCache *lru.Cache // Cache for the most recent block headers
	heightCache *lru.Cache // Cache for the most recent block height
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (hc *HeaderChain) CurrentHeader() *types.Header {
	return hc.currentHeader.Load().(*types.Header)
}

// NewHeaderChain creates a new HeaderChain structure.
//  getValidator should return the parent's validator
//  procInterrupt points to the parent's interrupt semaphore
//  wg points to the parent's shutdown wait group
func NewHeaderChain(chainDb kaidb.Database, config *configs.ChainConfig) (*HeaderChain, error) {
	log.Debug("NewHeaderChain")
	headerCache, _ := lru.New(headerCacheLimit)
	heightCache, _ := lru.New(heightCacheLimit)

	hc := &HeaderChain{
		config:      config,
		chainDb:     chainDb,
		headerCache: headerCache,
		heightCache: heightCache,
	}

	hc.genesisHeader = hc.GetHeaderByHeight(0)
	if hc.genesisHeader == nil {
		log.Debug("#debug30")
		return nil, ErrNoGenesis
	}
	log.Debug("#debug31")

	hc.currentHeader.Store(hc.genesisHeader)
	if head := rawdb.ReadHeadBlockHash(chainDb); head != (common.Hash{}) {
		if chead := hc.GetHeaderByHash(head); chead != nil {
			hc.currentHeader.Store(chead)
		}
	}
	hc.currentHeaderHash = hc.CurrentHeader().Hash()

	log.Debug("#debug32")
	return hc, nil
}

// GetHeaderByheight retrieves a block header from the database by height,
// caching it (associated with its hash) if found.
func (hc *HeaderChain) GetHeaderByHeight(height uint64) *types.Header {
	log.Debug("debug#20")
	hash := rawdb.ReadCanonicalHash(hc.chainDb, height)
	if hash == (common.Hash{}) {
		return nil
	}
	return hc.GetHeader(hash, height)
}

// GetHeader retrieves a block header from the database by hash and height,
// caching it if found.
func (hc *HeaderChain) GetHeader(hash common.Hash, height uint64) *types.Header {
	// Short circuit if the header's already in the cache, retrieve otherwise
	if header, ok := hc.headerCache.Get(hash); ok {
		return header.(*types.Header)
	}
	header := rawdb.ReadHeader(hc.chainDb, hash, height)
	if header == nil {
		return nil
	}
	// Cache the found header for next time and return
	hc.headerCache.Add(hash, header)
	return header
}

// GetHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
func (hc *HeaderChain) GetHeaderByHash(hash common.Hash) *types.Header {
	height := hc.GetBlockHeight(hash)
	if height == nil {
		return nil
	}
	return hc.GetHeader(hash, *height)
}

// GetBlockHeight retrieves the block height belonging to the given hash
// from the cache or database
func (hc *HeaderChain) GetBlockHeight(hash common.Hash) *uint64 {
	if cached, ok := hc.heightCache.Get(hash); ok {
		height := cached.(uint64)
		return &height
	}
	height := rawdb.ReadHeaderHeight(hc.chainDb, hash)
	if height != nil {
		hc.heightCache.Add(hash, *height)
	}
	return height
}

// SetCurrentHeader sets the current head header of the canonical chain.
func (hc *HeaderChain) SetCurrentHeader(head *types.Header) {
	rawdb.WriteHeadHeaderHash(hc.chainDb, head.Hash())

	hc.currentHeader.Store(head)
	hc.currentHeaderHash = head.Hash()
}

// SetGenesis sets a new genesis block header for the chain
func (hc *HeaderChain) SetGenesis(head *types.Header) {
	hc.genesisHeader = head
}

// DeleteCallback is a callback function that is called by SetHead before
// each header is deleted.
type DeleteCallback func(rawdb.DatabaseDeleter, common.Hash, uint64)

// SetHead rewinds the local chain to a new head. Everything above the new head
// will be deleted and the new one set.
func (hc *HeaderChain) SetHead(head uint64, delFn DeleteCallback) {
	height := uint64(0)

	if hdr := hc.CurrentHeader(); hdr != nil {
		height = hdr.Height
	}
	batch := hc.chainDb.NewBatch()
	for hdr := hc.CurrentHeader(); hdr != nil && hdr.Height > head; hdr = hc.CurrentHeader() {
		hash := hdr.Hash()
		height := hdr.Height
		if delFn != nil {
			delFn(batch, hash, height)
		}
		rawdb.DeleteHeader(batch, hash, height)

		hc.currentHeader.Store(hc.GetHeader(hdr.LastCommitHash, hdr.Height-1))
	}
	// Roll back the canonical chain numbering
	for i := height; i > head; i-- {
		rawdb.DeleteCanonicalHash(batch, i)
	}
	batch.Write()

	// Clear out any stale content from the caches
	hc.headerCache.Purge()
	hc.heightCache.Purge()

	if hc.CurrentHeader() == nil {
		hc.currentHeader.Store(hc.genesisHeader)
	}
	hc.currentHeaderHash = hc.CurrentHeader().Hash()

	rawdb.WriteHeadHeaderHash(hc.chainDb, hc.currentHeaderHash)
}
