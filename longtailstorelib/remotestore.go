package longtailstorelib

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DanEngelbrecht/golongtail/longtaillib"
	"github.com/pkg/errors"
)

func createBlobStoreForURI(uri string) (BlobStore, error) {
	blobStoreURL, err := url.Parse(uri)
	if err == nil {
		switch blobStoreURL.Scheme {
		case "gs":
			return NewGCSBlobStore(blobStoreURL)
		case "s3":
			return NewS3BlobStore(blobStoreURL)
		case "abfs":
			return nil, fmt.Errorf("azure Gen1 storage not yet implemented")
		case "abfss":
			return nil, fmt.Errorf("azure Gen2 storage not yet implemented")
		case "file":
			return NewFSBlobStore(blobStoreURL.Path[1:])
		}
	}

	return NewFSBlobStore(uri)
}

func splitURI(uri string) (string, string) {
	i := strings.LastIndex(uri, "/")
	if i == -1 {
		i = strings.LastIndex(uri, "\\")
	}
	if i == -1 {
		return "", uri
	}
	return uri[:i], uri[i+1:]
}

// ReadFromURI ...
func ReadFromURI(uri string) ([]byte, error) {
	uriParent, uriName := splitURI(uri)
	blobStore, err := createBlobStoreForURI(uriParent)
	if err != nil {
		return nil, err
	}
	client, err := blobStore.NewClient(context.Background())
	if err != nil {
		return nil, err
	}
	defer client.Close()
	object, err := client.NewObject(uriName)
	if err != nil {
		return nil, err
	}
	vbuffer, err := object.Read()
	if err != nil {
		return nil, err
	}
	return vbuffer, nil
}

// ReadFromURI ...
func WriteToURI(uri string, data []byte) error {
	uriParent, uriName := splitURI(uri)
	blobStore, err := createBlobStoreForURI(uriParent)
	if err != nil {
		return err
	}
	client, err := blobStore.NewClient(context.Background())
	if err != nil {
		return err
	}
	defer client.Close()
	object, err := client.NewObject(uriName)
	if err != nil {
		return err
	}
	_, err = object.Write(data)
	if err != nil {
		return err
	}
	return nil
}

// AccessType defines how we will access the data in the store
type AccessType int

const (
	// Init - read/write access with forced rebuild of store index
	Init AccessType = iota
	// ReadWrite - read/write access with optional rebuild of store index
	ReadWrite
	// ReadOnly - read only access
	ReadOnly
)

type putBlockMessage struct {
	storedBlock      longtaillib.Longtail_StoredBlock
	asyncCompleteAPI longtaillib.Longtail_AsyncPutStoredBlockAPI
}

type getBlockMessage struct {
	blockHash        uint64
	asyncCompleteAPI longtaillib.Longtail_AsyncGetStoredBlockAPI
}

type prefetchBlockMessage struct {
	blockHash uint64
}

type preflightGetMessage struct {
	blockHashes      []uint64
	asyncCompleteAPI longtaillib.Longtail_AsyncPreflightStartedAPI
}

type blockIndexMessage struct {
	blockIndex longtaillib.Longtail_BlockIndex
}

type getExistingContentMessage struct {
	chunkHashes          []uint64
	minBlockUsagePercent uint32
	asyncCompleteAPI     longtaillib.Longtail_AsyncGetExistingContentAPI
}

type pendingPrefetchedBlock struct {
	storedBlock       longtaillib.Longtail_StoredBlock
	completeCallbacks []longtaillib.Longtail_AsyncGetStoredBlockAPI
}

type remoteStore struct {
	jobAPI        longtaillib.Longtail_JobAPI
	blobStore     BlobStore
	defaultClient BlobClient

	workerCount int

	putBlockChan           chan putBlockMessage
	getBlockChan           chan getBlockMessage
	preflightGetChan       chan preflightGetMessage
	prefetchBlockChan      chan prefetchBlockMessage
	blockIndexChan         chan blockIndexMessage
	getExistingContentChan chan getExistingContentMessage
	workerFlushChan        chan int
	workerFlushReplyChan   chan int
	indexFlushChan         chan int
	indexFlushReplyChan    chan int
	workerErrorChan        chan error
	prefetchMemory         int64
	maxPrefetchMemory      int64

	fetchedBlocksSync sync.Mutex
	prefetchBlocks    map[uint64]*pendingPrefetchedBlock

	stats longtaillib.BlockStoreStats
}

// String() ...
func (s *remoteStore) String() string {
	return s.defaultClient.String()
}

func readBlobWithRetry(
	ctx context.Context,
	s *remoteStore,
	client BlobClient,
	key string) ([]byte, int, error) {
	retryCount := 0
	objHandle, err := client.NewObject(key)
	if err != nil {
		return nil, retryCount, err
	}
	exists, err := objHandle.Exists()
	if err != nil {
		return nil, retryCount, err
	}
	if !exists {
		return nil, retryCount, longtaillib.ErrENOENT
	}
	blobData, err := objHandle.Read()
	if err != nil {
		log.Printf("Retrying getBlob %s in store %s\n", key, s.String())
		retryCount++
		blobData, err = objHandle.Read()
	}
	if err != nil {
		log.Printf("Retrying 500 ms delayed getBlob %s in store %s\n", key, s.String())
		time.Sleep(500 * time.Millisecond)
		retryCount++
		blobData, err = objHandle.Read()
	}
	if err != nil {
		log.Printf("Retrying 2 s delayed getBlob %s in store %s\n", key, s.String())
		time.Sleep(2 * time.Second)
		retryCount++
		blobData, err = objHandle.Read()
	}

	if err != nil {
		return nil, retryCount, err
	}

	return blobData, retryCount, nil
}

func putStoredBlock(
	ctx context.Context,
	s *remoteStore,
	blobClient BlobClient,
	blockIndexMessages chan<- blockIndexMessage,
	storedBlock longtaillib.Longtail_StoredBlock) error {

	atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_PutStoredBlock_Count], 1)

	blockIndex := storedBlock.GetBlockIndex()
	blockHash := blockIndex.GetBlockHash()
	key := GetBlockPath("chunks", blockHash)
	objHandle, err := blobClient.NewObject(key)
	if err != nil {
		return err
	}
	if exists, err := objHandle.Exists(); err == nil && !exists {
		blob, errno := longtaillib.WriteStoredBlockToBuffer(storedBlock)
		if errno != 0 {
			return longtaillib.ErrnoToError(errno, longtaillib.ErrEIO)
		}

		ok, err := objHandle.Write(blob)
		if err != nil || !ok {
			log.Printf("Retrying putBlob %s in store %s\n", key, s.String())
			atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_PutStoredBlock_RetryCount], 1)
			ok, err = objHandle.Write(blob)
		}
		if err != nil || !ok {
			log.Printf("Retrying 500 ms delayed putBlob %s in store %s\n", key, s.String())
			time.Sleep(500 * time.Millisecond)
			atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_PutStoredBlock_RetryCount], 1)
			ok, err = objHandle.Write(blob)
		}
		if err != nil || !ok {
			log.Printf("Retrying 2 s delayed putBlob %s in store %s\n", key, s.String())
			time.Sleep(2 * time.Second)
			atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_PutStoredBlock_RetryCount], 1)
			ok, err = objHandle.Write(blob)
		}

		if err != nil || !ok {
			atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_PutStoredBlock_FailCount], 1)
			return longtaillib.ErrnoToError(errno, longtaillib.ErrEIO)
		}

		atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_PutStoredBlock_Byte_Count], (uint64)(len(blob)))
		atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_PutStoredBlock_Chunk_Count], (uint64)(blockIndex.GetChunkCount()))
	}

	blockIndexCopy, err := blockIndex.Copy()
	if err != nil {
		return err
	}
	blockIndexMessages <- blockIndexMessage{blockIndex: blockIndexCopy}
	return nil
}

func getStoredBlock(
	ctx context.Context,
	s *remoteStore,
	blobClient BlobClient,
	blockHash uint64) (longtaillib.Longtail_StoredBlock, error) {

	atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_GetStoredBlock_Count], 1)

	key := GetBlockPath("chunks", blockHash)

	storedBlockData, retryCount, err := readBlobWithRetry(ctx, s, blobClient, key)
	atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_GetStoredBlock_RetryCount], uint64(retryCount))

	if err != nil || storedBlockData == nil {
		atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_GetStoredBlock_FailCount], 1)
		return longtaillib.Longtail_StoredBlock{}, err
	}

	storedBlock, errno := longtaillib.ReadStoredBlockFromBuffer(storedBlockData)
	if errno != 0 {
		atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_GetStoredBlock_FailCount], 1)
		return longtaillib.Longtail_StoredBlock{}, longtaillib.ErrnoToError(errno, longtaillib.ErrEIO)
	}

	atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_GetStoredBlock_Byte_Count], (uint64)(len(storedBlockData)))
	blockIndex := storedBlock.GetBlockIndex()
	if blockIndex.GetBlockHash() != blockHash {
		atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_GetStoredBlock_FailCount], 1)
		return longtaillib.Longtail_StoredBlock{}, longtaillib.ErrnoToError(longtaillib.EBADF, longtaillib.ErrEBADF)
	}
	atomic.AddUint64(&s.stats.StatU64[longtaillib.Longtail_BlockStoreAPI_StatU64_GetStoredBlock_Chunk_Count], (uint64)(blockIndex.GetChunkCount()))
	return storedBlock, nil
}

func fetchBlock(
	ctx context.Context,
	s *remoteStore,
	client BlobClient,
	getMsg getBlockMessage) {
	s.fetchedBlocksSync.Lock()
	prefetchedBlock := s.prefetchBlocks[getMsg.blockHash]
	if prefetchedBlock != nil {
		storedBlock := prefetchedBlock.storedBlock
		if storedBlock.IsValid() {
			s.prefetchBlocks[getMsg.blockHash] = nil
			blockSize := -int64(storedBlock.GetBlockSize())
			atomic.AddInt64(&s.prefetchMemory, blockSize)
			s.fetchedBlocksSync.Unlock()
			getMsg.asyncCompleteAPI.OnComplete(storedBlock, 0)
			return
		}
		prefetchedBlock.completeCallbacks = append(prefetchedBlock.completeCallbacks, getMsg.asyncCompleteAPI)
		s.fetchedBlocksSync.Unlock()
		return
	}
	prefetchedBlock = &pendingPrefetchedBlock{storedBlock: longtaillib.Longtail_StoredBlock{}}
	s.prefetchBlocks[getMsg.blockHash] = prefetchedBlock
	s.fetchedBlocksSync.Unlock()
	storedBlock, getStoredBlockErr := getStoredBlock(ctx, s, client, getMsg.blockHash)
	s.fetchedBlocksSync.Lock()
	prefetchedBlock, exists := s.prefetchBlocks[getMsg.blockHash]
	if exists && prefetchedBlock == nil {
		storedBlock.Dispose()
		s.fetchedBlocksSync.Unlock()
		return
	}
	completeCallbacks := prefetchedBlock.completeCallbacks
	s.prefetchBlocks[getMsg.blockHash] = nil
	s.fetchedBlocksSync.Unlock()
	for _, c := range completeCallbacks {
		if getStoredBlockErr != nil {
			c.OnComplete(longtaillib.Longtail_StoredBlock{}, longtaillib.ErrorToErrno(getStoredBlockErr, longtaillib.EIO))
			continue
		}
		buf, errno := longtaillib.WriteStoredBlockToBuffer(storedBlock)
		if errno != 0 {
			c.OnComplete(longtaillib.Longtail_StoredBlock{}, errno)
			continue
		}
		blockCopy, errno := longtaillib.ReadStoredBlockFromBuffer(buf)
		if errno != 0 {
			c.OnComplete(longtaillib.Longtail_StoredBlock{}, errno)
			continue
		}
		c.OnComplete(blockCopy, 0)
	}
	getMsg.asyncCompleteAPI.OnComplete(storedBlock, longtaillib.ErrorToErrno(getStoredBlockErr, longtaillib.EIO))
}

func prefetchBlock(
	ctx context.Context,
	s *remoteStore,
	client BlobClient,
	prefetchMsg prefetchBlockMessage) {
	s.fetchedBlocksSync.Lock()
	_, exists := s.prefetchBlocks[prefetchMsg.blockHash]
	if exists {
		// Already pre-fetched
		s.fetchedBlocksSync.Unlock()
		return
	}
	prefetchedBlock := &pendingPrefetchedBlock{storedBlock: longtaillib.Longtail_StoredBlock{}}
	s.prefetchBlocks[prefetchMsg.blockHash] = prefetchedBlock
	s.fetchedBlocksSync.Unlock()

	storedBlock, getErr := getStoredBlock(ctx, s, client, prefetchMsg.blockHash)
	if getErr != nil {
		return
	}

	s.fetchedBlocksSync.Lock()

	prefetchedBlock, exists = s.prefetchBlocks[prefetchMsg.blockHash]
	if prefetchedBlock == nil {
		storedBlock.Dispose()
		s.fetchedBlocksSync.Unlock()
		return
	}
	completeCallbacks := prefetchedBlock.completeCallbacks
	if len(completeCallbacks) == 0 {
		// Nobody is actively waiting for the block
		blockSize := int64(storedBlock.GetBlockSize())
		prefetchedBlock.storedBlock = storedBlock
		atomic.AddInt64(&s.prefetchMemory, blockSize)
		s.fetchedBlocksSync.Unlock()
		return
	}
	s.prefetchBlocks[prefetchMsg.blockHash] = nil
	s.fetchedBlocksSync.Unlock()
	for i := 1; i < len(completeCallbacks)-1; i++ {
		c := completeCallbacks[i]
		if getErr != nil {
			c.OnComplete(longtaillib.Longtail_StoredBlock{}, longtaillib.ErrorToErrno(getErr, longtaillib.EIO))
			continue
		}
		buf, errno := longtaillib.WriteStoredBlockToBuffer(storedBlock)
		if errno != 0 {
			c.OnComplete(longtaillib.Longtail_StoredBlock{}, errno)
			continue
		}
		blockCopy, errno := longtaillib.ReadStoredBlockFromBuffer(buf)
		if errno != 0 {
			c.OnComplete(longtaillib.Longtail_StoredBlock{}, errno)
			continue
		}
		c.OnComplete(blockCopy, 0)
	}
	completeCallbacks[0].OnComplete(storedBlock, longtaillib.ErrorToErrno(getErr, longtaillib.EIO))
}

func flushPrefetch(
	s *remoteStore,
	prefetchBlockChan <-chan prefetchBlockMessage) {

L:
	for {
		select {
		case <-prefetchBlockChan:
		default:
			break L
		}
	}

	s.fetchedBlocksSync.Lock()
	flushBlocks := []uint64{}
	for k, v := range s.prefetchBlocks {
		if v != nil && len(v.completeCallbacks) > 0 {
			fmt.Printf("Somebody is still waiting for prefetch %d\n", k)
			continue
		}
		flushBlocks = append(flushBlocks, k)
	}
	for _, h := range flushBlocks {
		b := s.prefetchBlocks[h]
		if b != nil {
			if b.storedBlock.IsValid() {
				blockSize := -int64(b.storedBlock.GetBlockSize())
				atomic.AddInt64(&s.prefetchMemory, blockSize)
				b.storedBlock.Dispose()
			}
		}
		delete(s.prefetchBlocks, h)
	}
	s.fetchedBlocksSync.Unlock()
}

func remoteWorker(
	ctx context.Context,
	s *remoteStore,
	putBlockMessages <-chan putBlockMessage,
	getBlockMessages <-chan getBlockMessage,
	prefetchBlockChan <-chan prefetchBlockMessage,
	blockIndexMessages chan<- blockIndexMessage,
	flushMessages <-chan int,
	flushReplyMessages chan<- int,
	accessType AccessType) error {
	client, err := s.blobStore.NewClient(ctx)
	if err != nil {
		return errors.Wrap(err, s.blobStore.String())
	}
	defer client.Close()
	run := true
	for run {
		received := 0
		select {
		case putMsg, more := <-putBlockMessages:
			if more {
				received++
				if accessType == ReadOnly {
					putMsg.asyncCompleteAPI.OnComplete(longtaillib.EACCES)
					continue
				}
				err := putStoredBlock(ctx, s, client, blockIndexMessages, putMsg.storedBlock)
				putMsg.asyncCompleteAPI.OnComplete(longtaillib.ErrorToErrno(err, longtaillib.EIO))
			} else {
				run = false
			}
		case getMsg := <-getBlockMessages:
			received++
			fetchBlock(ctx, s, client, getMsg)
		default:
		}
		if received == 0 {
			if s.prefetchMemory < s.maxPrefetchMemory {
				select {
				case <-flushMessages:
					flushPrefetch(s, prefetchBlockChan)
					flushReplyMessages <- 0
				case putMsg, more := <-putBlockMessages:
					if more {
						if accessType == ReadOnly {
							putMsg.asyncCompleteAPI.OnComplete(longtaillib.EACCES)
							continue
						}
						err := putStoredBlock(ctx, s, client, blockIndexMessages, putMsg.storedBlock)
						putMsg.asyncCompleteAPI.OnComplete(longtaillib.ErrorToErrno(err, longtaillib.EIO))
					} else {
						run = false
					}
				case getMsg := <-getBlockMessages:
					fetchBlock(ctx, s, client, getMsg)
				case prefetchMsg := <-prefetchBlockChan:
					prefetchBlock(ctx, s, client, prefetchMsg)
				}
			} else {
				select {
				case <-flushMessages:
					flushPrefetch(s, prefetchBlockChan)
					flushReplyMessages <- 0
				case putMsg, more := <-putBlockMessages:
					if more {
						if accessType == ReadOnly {
							putMsg.asyncCompleteAPI.OnComplete(longtaillib.EACCES)
							continue
						}
						err := putStoredBlock(ctx, s, client, blockIndexMessages, putMsg.storedBlock)
						putMsg.asyncCompleteAPI.OnComplete(longtaillib.ErrorToErrno(err, longtaillib.EIO))
					} else {
						run = false
					}
				case getMsg := <-getBlockMessages:
					fetchBlock(ctx, s, client, getMsg)
				}
			}
		}
	}

	flushPrefetch(s, prefetchBlockChan)
	return nil
}

func tryUpdateRemoteStoreIndex(
	ctx context.Context,
	updatedStoreIndex longtaillib.Longtail_StoreIndex,
	objHandle BlobObject) (bool, longtaillib.Longtail_StoreIndex, error) {

	exists, err := objHandle.LockWriteVersion()
	if err != nil {
		return false, longtaillib.Longtail_StoreIndex{}, err
	}
	if exists {
		blob, err := objHandle.Read()
		if err != nil {
			return false, longtaillib.Longtail_StoreIndex{}, errors.Wrapf(err, "updateRemoteStoreIndex: objHandle.Read() failed")
		}

		remoteStoreIndex, errno := longtaillib.ReadStoreIndexFromBuffer(blob)
		if errno != 0 {
			return false, longtaillib.Longtail_StoreIndex{}, errors.Wrapf(longtaillib.ErrnoToError(errno, longtaillib.ErrEIO), "updateRemoteStoreIndex: longtaillib.ReadStoreIndexFromBuffer() failed")
		}
		defer remoteStoreIndex.Dispose()

		newStoreIndex, errno := longtaillib.MergeStoreIndex(updatedStoreIndex, remoteStoreIndex)
		if errno != 0 {
			return false, longtaillib.Longtail_StoreIndex{}, errors.Wrapf(longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM), "updateRemoteStoreIndex: longtaillib.MergeStoreIndex() failed")
		}

		storeBlob, errno := longtaillib.WriteStoreIndexToBuffer(newStoreIndex)
		if errno != 0 {
			newStoreIndex.Dispose()
			return false, longtaillib.Longtail_StoreIndex{}, errors.Wrapf(longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM), "updateRemoteStoreIndex: longtaillib.WriteStoreIndexToBuffer() kfailed")
		}

		ok, err := objHandle.Write(storeBlob)
		if err != nil {
			newStoreIndex.Dispose()
			return false, longtaillib.Longtail_StoreIndex{}, errors.Wrapf(err, "updateRemoteStoreIndex: objHandle.Write() failed")
		}
		if !ok {
			newStoreIndex.Dispose()
			return false, longtaillib.Longtail_StoreIndex{}, nil
		}
		return ok, newStoreIndex, nil
	}
	storeBlob, errno := longtaillib.WriteStoreIndexToBuffer(updatedStoreIndex)
	if errno != 0 {
		return false, longtaillib.Longtail_StoreIndex{}, errors.Wrapf(longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM), "updateRemoteStoreIndex: WriteStoreIndexToBuffer() failed")
	}

	ok, err := objHandle.Write(storeBlob)
	if err != nil {
		return false, longtaillib.Longtail_StoreIndex{}, errors.Wrapf(err, "updateRemoteStoreIndex: objHandle.Write() failed")
	}
	return ok, longtaillib.Longtail_StoreIndex{}, nil
}

func updateRemoteStoreIndex(
	ctx context.Context,
	blobClient BlobClient,
	updatedStoreIndex longtaillib.Longtail_StoreIndex) (longtaillib.Longtail_StoreIndex, error) {

	key := "store.lsi"
	objHandle, err := blobClient.NewObject(key)
	if err != nil {
		return longtaillib.Longtail_StoreIndex{}, errors.Wrapf(err, "updateRemoteStoreIndex: blobClient.NewObject(%s) failed", key)
	}
	for {
		ok, newStoreIndex, err := tryUpdateRemoteStoreIndex(
			ctx,
			updatedStoreIndex,
			objHandle)
		if ok {
			return newStoreIndex, nil
		}
		if err != nil {
			return longtaillib.Longtail_StoreIndex{}, errors.Wrapf(err, "updateRemoteStoreIndex: tryUpdateRemoteStoreIndex(%s) failed", key)
		}
		log.Printf("Retrying updating remote store index %s\n", key)
	}
	return longtaillib.Longtail_StoreIndex{}, nil
}

func getStoreIndexFromBlocks(
	ctx context.Context,
	s *remoteStore,
	blobClient BlobClient,
	blockKeys []string) (longtaillib.Longtail_StoreIndex, error) {

	storeIndex, errno := longtaillib.CreateStoreIndexFromBlocks([]longtaillib.Longtail_BlockIndex{})
	if errno != 0 {
		return longtaillib.Longtail_StoreIndex{}, longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM)
	}

	batchCount := s.workerCount
	batchStart := 0

	if batchCount > len(blockKeys) {
		batchCount = len(blockKeys)
	}
	clients := make([]BlobClient, batchCount)
	for c := 0; c < batchCount; c++ {
		client, err := s.blobStore.NewClient(ctx)
		if err != nil {
			storeIndex.Dispose()
			return longtaillib.Longtail_StoreIndex{}, err
		}
		clients[c] = client
	}

	var wg sync.WaitGroup

	for batchStart < len(blockKeys) {
		batchLength := batchCount
		if batchStart+batchLength > len(blockKeys) {
			batchLength = len(blockKeys) - batchStart
		}
		batchBlockIndexes := make([]longtaillib.Longtail_BlockIndex, batchLength)
		wg.Add(batchLength)
		for batchPos := 0; batchPos < batchLength; batchPos++ {
			i := batchStart + batchPos
			blockKey := blockKeys[i]
			go func(client BlobClient, batchPos int, blockKey string) {
				storedBlockData, _, err := readBlobWithRetry(
					ctx,
					s,
					client,
					blockKey)

				if err != nil {
					wg.Done()
					return
				}

				blockIndex, errno := longtaillib.ReadBlockIndexFromBuffer(storedBlockData)
				if errno != 0 {
					wg.Done()
					return
				}

				blockPath := GetBlockPath("chunks", blockIndex.GetBlockHash())
				if blockPath == blockKey {
					batchBlockIndexes[batchPos] = blockIndex
				} else {
					log.Printf("Block %s name does not match content hash, expected name %s\n", blockKey, blockPath)
				}

				wg.Done()
			}(clients[batchPos], batchPos, blockKey)
		}
		wg.Wait()
		writeIndex := 0
		for i, blockIndex := range batchBlockIndexes {
			if !blockIndex.IsValid() {
				continue
			}
			if i > writeIndex {
				batchBlockIndexes[writeIndex] = blockIndex
			}
			writeIndex++
		}
		batchBlockIndexes = batchBlockIndexes[:writeIndex]
		batchStoreIndex, errno := longtaillib.CreateStoreIndexFromBlocks(batchBlockIndexes)
		for _, blockIndex := range batchBlockIndexes {
			blockIndex.Dispose()
		}
		if errno != 0 {
			batchStoreIndex.Dispose()
			storeIndex.Dispose()
			return longtaillib.Longtail_StoreIndex{}, longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM)
		}
		newStoreIndex, errno := longtaillib.MergeStoreIndex(storeIndex, batchStoreIndex)
		if errno != 0 {
			batchStoreIndex.Dispose()
			storeIndex.Dispose()
			return longtaillib.Longtail_StoreIndex{}, longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM)
		}
		batchStoreIndex.Dispose()
		storeIndex.Dispose()
		storeIndex = newStoreIndex
		//		blockIndexes = append(blockIndexes, batchBlockIndexes[:writeIndex]...)
		batchStart += batchLength
		log.Printf("Scanned %d/%d blocks in %s\n", batchStart, len(blockKeys), blobClient.String())
	}

	for c := 0; c < batchCount; c++ {
		clients[c].Close()
	}

	return storeIndex, nil
}

func buildStoreIndexFromStoreBlocks(
	ctx context.Context,
	s *remoteStore,
	blobClient BlobClient) (longtaillib.Longtail_StoreIndex, error) {

	var items []string
	blobs, err := blobClient.GetObjects()
	if err != nil {
		return longtaillib.Longtail_StoreIndex{}, err
	}

	for _, blob := range blobs {
		if blob.Size == 0 {
			continue
		}
		if strings.HasSuffix(blob.Name, ".lsb") {
			items = append(items, blob.Name)
		}
	}

	return getStoreIndexFromBlocks(ctx, s, blobClient, items)
}

func storeIndexWorkerReplyErrorState(
	blockIndexMessages <-chan blockIndexMessage,
	getExistingContentMessages <-chan getExistingContentMessage,
	flushMessages <-chan int,
	flushReplyMessages chan<- int) {
	for {
		select {
		case <-flushMessages:
			flushReplyMessages <- 0
		case _, more := <-blockIndexMessages:
			if !more {
				return
			}
		case getExistingContentMessage := <-getExistingContentMessages:
			getExistingContentMessage.asyncCompleteAPI.OnComplete(longtaillib.Longtail_StoreIndex{}, longtaillib.EINVAL)
		}
	}
}

func readStoreStoreIndex(
	ctx context.Context,
	s *remoteStore,
	client BlobClient) (longtaillib.Longtail_StoreIndex, error) {

	key := "store.lsi"
	blobData, _, err := readBlobWithRetry(ctx, s, client, key)
	if err != nil {
		return longtaillib.Longtail_StoreIndex{}, err
	}
	if blobData == nil {
		return longtaillib.Longtail_StoreIndex{}, nil
	}
	storeIndex, errno := longtaillib.ReadStoreIndexFromBuffer(blobData)
	if errno != 0 {
		return longtaillib.Longtail_StoreIndex{}, errors.Wrapf(longtaillib.ErrnoToError(errno, longtaillib.ErrEIO), "contentIndexWorker: longtaillib.ReadStoreIndexFromBuffer() for %s", key)
	}
	return storeIndex, nil
}

func onPreflighMessage(
	s *remoteStore,
	storeIndex longtaillib.Longtail_StoreIndex,
	message preflightGetMessage,
	prefetchBlockMessages chan<- prefetchBlockMessage) {

	for _, blockHash := range message.blockHashes {
		prefetchBlockMessages <- prefetchBlockMessage{blockHash: blockHash}
	}
	message.asyncCompleteAPI.OnComplete(message.blockHashes, 0)
}

func onGetExistingContentMessage(
	s *remoteStore,
	storeIndex longtaillib.Longtail_StoreIndex,
	message getExistingContentMessage) {
	existingStoreIndex, errno := longtaillib.GetExistingStoreIndex(storeIndex, message.chunkHashes, message.minBlockUsagePercent)
	if errno != 0 {
		message.asyncCompleteAPI.OnComplete(longtaillib.Longtail_StoreIndex{}, errno)
		return
	}
	message.asyncCompleteAPI.OnComplete(existingStoreIndex, 0)
}

func updateStoreIndex(
	storeIndex longtaillib.Longtail_StoreIndex,
	addedBlockIndexes []longtaillib.Longtail_BlockIndex) (longtaillib.Longtail_StoreIndex, error) {
	addedStoreIndex, errno := longtaillib.CreateStoreIndexFromBlocks(addedBlockIndexes)
	if errno != 0 {
		return longtaillib.Longtail_StoreIndex{}, errors.Wrap(longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM), "contentIndexWorker: longtaillib.CreateStoreIndexFromBlocks() failed")
	}

	if !storeIndex.IsValid() {
		return addedStoreIndex, nil
	}
	updatedStoreIndex, errno := longtaillib.MergeStoreIndex(addedStoreIndex, storeIndex)
	addedStoreIndex.Dispose()
	if errno != 0 {
		updatedStoreIndex.Dispose()
		return longtaillib.Longtail_StoreIndex{}, errors.Wrap(longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM), "contentIndexWorker: longtaillib.MergeStoreIndex() failed")
	}
	return updatedStoreIndex, nil
}

func getStoreIndex(
	ctx context.Context,
	s *remoteStore,
	optionalStoreIndexPath string,
	client BlobClient,
	accessType AccessType,
	storeIndex longtaillib.Longtail_StoreIndex,
	saveStoreIndex bool,
	addedBlockIndexes []longtaillib.Longtail_BlockIndex) (longtaillib.Longtail_StoreIndex, bool, error) {
	var err error
	var errno int
	if !storeIndex.IsValid() {
		if accessType == Init {
			saveStoreIndex = true
		} else {
			if accessType == ReadOnly && len(optionalStoreIndexPath) > 0 {
				sbuffer, err := ReadFromURI(optionalStoreIndexPath)
				if err == nil {
					storeIndex, errno = longtaillib.ReadStoreIndexFromBuffer(sbuffer)
					if errno != 0 {
						log.Printf("Failed parsing local store index from %s: %d\n", optionalStoreIndexPath, errno)
					}
				} else {
					log.Printf("Failed reading local store index: %v\n", err)
				}
			}
			if !storeIndex.IsValid() {
				storeIndex, err = readStoreStoreIndex(ctx, s, client)
				if err != nil {
					log.Printf("contentIndexWorker: readStoreStoreIndex() failed with %v", err)
				}
			}
		}

		if !storeIndex.IsValid() {
			if accessType == ReadOnly {
				storeIndex, errno = longtaillib.CreateStoreIndexFromBlocks([]longtaillib.Longtail_BlockIndex{})
				if errno != 0 {
					return longtaillib.Longtail_StoreIndex{}, false, errors.Wrapf(longtaillib.ErrnoToError(longtaillib.EACCES, longtaillib.ErrEACCES), "contentIndexWorker: CreateStoreIndexFromBlocks() failed")
				}
			} else {
				storeIndex, err = buildStoreIndexFromStoreBlocks(
					ctx,
					s,
					client)

				if err != nil {
					return longtaillib.Longtail_StoreIndex{}, false, errors.Wrapf(longtaillib.ErrnoToError(errno, longtaillib.ErrENOMEM), "contentIndexWorker: buildStoreIndexFromStoreBlocks() failed")
				}
				log.Printf("Rebuilt remote index with %d blocks\n", len(storeIndex.GetBlockHashes()))
				newStoreIndex, err := updateRemoteStoreIndex(ctx, client, storeIndex)
				if err != nil {
					log.Printf("Failed to update store index in store %s\n", s.String())
					saveStoreIndex = true
				}
				if newStoreIndex.IsValid() {
					storeIndex.Dispose()
					storeIndex = newStoreIndex
				}
			}
		}
	}

	if len(addedBlockIndexes) > 0 {
		updatedStoreIndex, err := updateStoreIndex(storeIndex, addedBlockIndexes)
		if err != nil {
			log.Printf("WARNING: Failed to update store index with added blocks %v", err)
			return longtaillib.Longtail_StoreIndex{}, false, err
		}
		storeIndex.Dispose()
		storeIndex = updatedStoreIndex
		saveStoreIndex = true
		addedBlockIndexes = nil
	}
	return storeIndex, saveStoreIndex, nil
}

func contentIndexWorker(
	ctx context.Context,
	s *remoteStore,
	optionalStoreIndexPath string,
	preflightGetMessages <-chan preflightGetMessage,
	prefetchBlockMessages chan<- prefetchBlockMessage,
	blockIndexMessages <-chan blockIndexMessage,
	getExistingContentMessages <-chan getExistingContentMessage,
	flushMessages <-chan int,
	flushReplyMessages chan<- int,
	accessType AccessType) error {

	client, err := s.blobStore.NewClient(ctx)
	if err != nil {
		storeIndexWorkerReplyErrorState(blockIndexMessages, getExistingContentMessages, flushMessages, flushReplyMessages)
		return errors.Wrap(err, s.blobStore.String())
	}
	defer client.Close()

	saveStoreIndex := false

	storeIndex := longtaillib.Longtail_StoreIndex{}

	var addedBlockIndexes []longtaillib.Longtail_BlockIndex
	defer func(addedBlockIndexes []longtaillib.Longtail_BlockIndex) {
		for _, blockIndex := range addedBlockIndexes {
			blockIndex.Dispose()
		}
	}(addedBlockIndexes)

	run := true
	for run {
		received := 0
		select {
		case preflightGetMsg := <-preflightGetMessages:
			received++
			storeIndex, saveStoreIndex, err = getStoreIndex(
				ctx,
				s,
				optionalStoreIndexPath,
				client,
				accessType,
				storeIndex,
				saveStoreIndex,
				addedBlockIndexes)
			if err != nil {
				storeIndex.Dispose()
				preflightGetMsg.asyncCompleteAPI.OnComplete([]uint64{}, longtaillib.ErrorToErrno(err, longtaillib.EIO))
				storeIndexWorkerReplyErrorState(blockIndexMessages, getExistingContentMessages, flushMessages, flushReplyMessages)
				return err
			}
			onPreflighMessage(s, storeIndex, preflightGetMsg, prefetchBlockMessages)
		case blockIndexMsg, more := <-blockIndexMessages:
			if more {
				received++
				addedBlockIndexes = append(addedBlockIndexes, blockIndexMsg.blockIndex)
			} else {
				run = false
			}
		case getExistingContentMessage := <-getExistingContentMessages:
			received++
			storeIndex, saveStoreIndex, err = getStoreIndex(
				ctx,
				s,
				optionalStoreIndexPath,
				client,
				accessType,
				storeIndex,
				saveStoreIndex,
				addedBlockIndexes)
			if err != nil {
				storeIndex.Dispose()
				getExistingContentMessage.asyncCompleteAPI.OnComplete(longtaillib.Longtail_StoreIndex{}, longtaillib.ErrorToErrno(err, longtaillib.EIO))
				storeIndexWorkerReplyErrorState(blockIndexMessages, getExistingContentMessages, flushMessages, flushReplyMessages)
				return err
			}
			onGetExistingContentMessage(s, storeIndex, getExistingContentMessage)
		default:
		}

		if received > 0 {
			continue
		}

		select {
		case <-flushMessages:
			if len(addedBlockIndexes) > 0 && accessType != ReadOnly {
				updatedStoreIndex, err := updateStoreIndex(storeIndex, addedBlockIndexes)
				if err != nil {
					flushReplyMessages <- longtaillib.ErrorToErrno(err, longtaillib.ENOMEM)
					continue
				}
				storeIndex.Dispose()
				storeIndex = updatedStoreIndex
				addedBlockIndexes = nil
				saveStoreIndex = true
			}
			if saveStoreIndex {
				newStoreIndex, err := updateRemoteStoreIndex(ctx, client, storeIndex)
				if err != nil {
					flushReplyMessages <- longtaillib.ErrorToErrno(err, longtaillib.ENOMEM)
					continue
				}
				if newStoreIndex.IsValid() {
					storeIndex.Dispose()
					storeIndex = newStoreIndex
				}
				saveStoreIndex = false
			}
			flushReplyMessages <- 0
		case preflightGetMsg := <-preflightGetMessages:
			storeIndex, saveStoreIndex, err = getStoreIndex(
				ctx,
				s,
				optionalStoreIndexPath,
				client,
				accessType,
				storeIndex,
				saveStoreIndex,
				addedBlockIndexes)
			if err != nil {
				storeIndex.Dispose()
				preflightGetMsg.asyncCompleteAPI.OnComplete([]uint64{}, longtaillib.ErrorToErrno(err, longtaillib.EIO))
				storeIndexWorkerReplyErrorState(blockIndexMessages, getExistingContentMessages, flushMessages, flushReplyMessages)
				return err
			}
			onPreflighMessage(s, storeIndex, preflightGetMsg, prefetchBlockMessages)
		case blockIndexMsg, more := <-blockIndexMessages:
			if more {
				addedBlockIndexes = append(addedBlockIndexes, blockIndexMsg.blockIndex)
			} else {
				run = false
			}
		case getExistingContentMessage := <-getExistingContentMessages:
			storeIndex, saveStoreIndex, err = getStoreIndex(
				ctx,
				s,
				optionalStoreIndexPath,
				client,
				accessType,
				storeIndex,
				saveStoreIndex,
				addedBlockIndexes)
			if err != nil {
				storeIndex.Dispose()
				getExistingContentMessage.asyncCompleteAPI.OnComplete(longtaillib.Longtail_StoreIndex{}, longtaillib.ErrorToErrno(err, longtaillib.EIO))
				storeIndexWorkerReplyErrorState(blockIndexMessages, getExistingContentMessages, flushMessages, flushReplyMessages)
				return err
			}
			onGetExistingContentMessage(s, storeIndex, getExistingContentMessage)
		}
	}

	if accessType == ReadOnly {
		storeIndex.Dispose()
		return nil
	}

	if len(addedBlockIndexes) > 0 {
		updatedStoreIndex, err := updateStoreIndex(storeIndex, addedBlockIndexes)
		if err != nil {
			return errors.Wrapf(err, "WARNING: Failed to update store index with added blocks")
		}
		storeIndex.Dispose()
		storeIndex = updatedStoreIndex
		saveStoreIndex = true
		addedBlockIndexes = nil
	}

	if saveStoreIndex {
		newIndex, err := updateRemoteStoreIndex(ctx, client, storeIndex)
		storeIndex.Dispose()
		if err != nil {
			return err
		}
		newIndex.Dispose()
	}
	return nil
}

// NewRemoteBlockStore ...
func NewRemoteBlockStore(
	jobAPI longtaillib.Longtail_JobAPI,
	blobStore BlobStore,
	optionalStoreIndexPath string,
	workerCount int,
	accessType AccessType) (longtaillib.BlockStoreAPI, error) {
	ctx := context.Background()
	defaultClient, err := blobStore.NewClient(ctx)
	if err != nil {
		return nil, errors.Wrap(err, blobStore.String())
	}

	s := &remoteStore{
		jobAPI:        jobAPI,
		blobStore:     blobStore,
		defaultClient: defaultClient}

	s.workerCount = workerCount
	s.putBlockChan = make(chan putBlockMessage, s.workerCount*8)
	s.getBlockChan = make(chan getBlockMessage, s.workerCount*2048)
	s.prefetchBlockChan = make(chan prefetchBlockMessage, s.workerCount*2048)
	s.preflightGetChan = make(chan preflightGetMessage, 16)
	s.blockIndexChan = make(chan blockIndexMessage, s.workerCount*2048)
	s.getExistingContentChan = make(chan getExistingContentMessage, 16)
	s.workerFlushChan = make(chan int, s.workerCount)
	s.workerFlushReplyChan = make(chan int, s.workerCount)
	s.indexFlushChan = make(chan int, 1)
	s.indexFlushReplyChan = make(chan int, 1)
	s.workerErrorChan = make(chan error, 1+s.workerCount)

	s.prefetchMemory = 0
	s.maxPrefetchMemory = 512 * 1024 * 1024

	s.prefetchBlocks = map[uint64]*pendingPrefetchedBlock{}

	go func() {
		err := contentIndexWorker(ctx, s, optionalStoreIndexPath, s.preflightGetChan, s.prefetchBlockChan, s.blockIndexChan, s.getExistingContentChan, s.indexFlushChan, s.indexFlushReplyChan, accessType)
		s.workerErrorChan <- err
	}()

	for i := 0; i < s.workerCount; i++ {
		go func() {
			err := remoteWorker(ctx, s, s.putBlockChan, s.getBlockChan, s.prefetchBlockChan, s.blockIndexChan, s.workerFlushChan, s.workerFlushReplyChan, accessType)
			s.workerErrorChan <- err
		}()
	}

	return s, nil
}

// GetBlockPath ...
func GetBlockPath(basePath string, blockHash uint64) string {
	fileName := fmt.Sprintf("0x%016x.lsb", blockHash)
	dir := filepath.Join(basePath, fileName[2:6])
	name := filepath.Join(dir, fileName)
	name = strings.Replace(name, "\\", "/", -1)
	return name
}

// PutStoredBlock ...
func (s *remoteStore) PutStoredBlock(storedBlock longtaillib.Longtail_StoredBlock, asyncCompleteAPI longtaillib.Longtail_AsyncPutStoredBlockAPI) int {
	s.putBlockChan <- putBlockMessage{storedBlock: storedBlock, asyncCompleteAPI: asyncCompleteAPI}
	return 0
}

// PreflightGet ...
func (s *remoteStore) PreflightGet(blockHashes []uint64, asyncCompleteAPI longtaillib.Longtail_AsyncPreflightStartedAPI) int {
	s.preflightGetChan <- preflightGetMessage{blockHashes: blockHashes, asyncCompleteAPI: asyncCompleteAPI}
	return 0
}

// GetStoredBlock ...
func (s *remoteStore) GetStoredBlock(blockHash uint64, asyncCompleteAPI longtaillib.Longtail_AsyncGetStoredBlockAPI) int {
	s.getBlockChan <- getBlockMessage{blockHash: blockHash, asyncCompleteAPI: asyncCompleteAPI}
	return 0
}

// GetExistingContent ...
func (s *remoteStore) GetExistingContent(
	chunkHashes []uint64,
	minBlockUsagePercent uint32,
	asyncCompleteAPI longtaillib.Longtail_AsyncGetExistingContentAPI) int {
	s.getExistingContentChan <- getExistingContentMessage{chunkHashes: chunkHashes, minBlockUsagePercent: minBlockUsagePercent, asyncCompleteAPI: asyncCompleteAPI}
	return 0
}

// GetStats ...
func (s *remoteStore) GetStats() (longtaillib.BlockStoreStats, int) {
	return s.stats, 0
}

// Flush ...
func (s *remoteStore) Flush(asyncCompleteAPI longtaillib.Longtail_AsyncFlushAPI) int {
	go func() {
		any_errno := 0
		for i := 0; i < s.workerCount; i++ {
			s.workerFlushChan <- 1
		}
		for i := 0; i < s.workerCount; i++ {
			errno := <-s.workerFlushReplyChan
			if errno != 0 && any_errno == 0 {
				any_errno = errno
			}
		}
		s.indexFlushChan <- 1
		errno := <-s.indexFlushReplyChan
		if errno != 0 && any_errno == 0 {
			any_errno = errno
		}
		asyncCompleteAPI.OnComplete(any_errno)
	}()
	return 0
}

// Close ...
func (s *remoteStore) Close() {
	close(s.putBlockChan)
	for i := 0; i < s.workerCount; i++ {
		err := <-s.workerErrorChan
		if err != nil {
			log.Fatal(err)
		}
	}
	close(s.blockIndexChan)
	err := <-s.workerErrorChan
	if err != nil {
		log.Fatal(err)
	}

	s.defaultClient.Close()
}
