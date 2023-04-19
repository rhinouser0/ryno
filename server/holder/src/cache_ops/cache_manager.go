// //////////////////////////////
// 2022 SHLab all rights reserved
// //////////////////////////////

package cache_ops

import (
	"bytes"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	blob "holder/src/blob_handler"
	db_ops "holder/src/db_ops"
	"holder/src/file_handler"

	"github.com/common/definition"
	. "github.com/common/zaplog"
	"go.uber.org/zap"
)

// TODO:
// 1. Need GC db metadata loop
// 2. Need optimize the OSS download
// 3. Need improving the cache eviction algorithm
type CacheManager struct {
	wMtx sync.Mutex
	// fileName->fid
	// TODO: currently fid is fileName, so we can refactor writeItemMap.
	writeItemMap map[string]string
	wQueue       []string

	pMtx         sync.Mutex
	purgeItemMap map[string]time.Time
	pQueue       []string

	dbOpsFile *db_ops.DBOpsFile
	pbh       *blob.PhyBH
}

func (mgr *CacheManager) New(fdb *db_ops.DBOpsFile, bh *blob.PhyBH) {
	mgr.writeItemMap = make(map[string]string)
	mgr.purgeItemMap = make(map[string]time.Time)
	mgr.wQueue = make([]string, 0)
	mgr.pQueue = make([]string, 0)
	mgr.dbOpsFile = fdb
	mgr.pbh = bh

	// Dispatch background thread.
	go mgr.loopBatchWrite()
	go mgr.loopGarbageCollection()
}

func (mgr *CacheManager) EnqueueWriteReq(
	fid string, fileName string) {
	mgr.wMtx.Lock()
	defer mgr.wMtx.Unlock()
	_, exist := mgr.writeItemMap[fileName]
	if exist {
		return
	}
	mgr.writeItemMap[fileName] = fid
	mgr.wQueue = append(mgr.wQueue, fileName)
}

// Assuming with lock.
func (mgr *CacheManager) EnqueueDeletionReq() {
	mgr.pMtx.Lock()
	defer mgr.pMtx.Unlock()
	tpltId, err := mgr.pbh.GetTailNameForEvict()
	if err != nil {
		return
	}
	err = mgr.dbOpsFile.DeleteFileWithTripleIdInDB(tpltId)
	if err != nil {
		ZapLogger.Error("DELETE FILE IN DB ERROR", zap.Any("error", err))
		return
	}
	mgr.purgeItemMap[tpltId] = time.Now()
	mgr.pQueue = append(mgr.pQueue, tpltId)
}

func (mgr *CacheManager) loopBatchWrite() {
	for {
		time.Sleep(200 * time.Millisecond)

		mgr.wMtx.Lock()
		if len(mgr.wQueue) == 0 {
			mgr.wMtx.Unlock()
			continue
		}
		numToFetch := min(definition.F_num_batch_write, len(mgr.wQueue))
		var namesAtHand = mgr.wQueue[:numToFetch]
		mgr.wQueue = mgr.wQueue[numToFetch:]

		var fids []string
		for _, name := range namesAtHand {
			fids = append(fids, mgr.writeItemMap[name])
			delete(mgr.writeItemMap, name)
		}
		mgr.wMtx.Unlock()

		// Start handling jobs. This will block the main job pulling thread.
		wg := &sync.WaitGroup{}
		for i, fileName := range namesAtHand {
			wg.Add(1)
			go func(filename string, fid string) {
				mgr.dowloadAndWriteCache(filename, fid)
				wg.Done()
			}(fileName, fids[i])
		}
		wg.Wait()
	}
}

func (mgr *CacheManager) loopGarbageCollection() {
	for {
		time.Sleep(200 * time.Millisecond)
		mgr.pMtx.Lock()
		if len(mgr.purgeItemMap) == 0 {
			mgr.pMtx.Unlock()
			continue
		}
		wg := &sync.WaitGroup{}
		for _, tpltId := range mgr.pQueue {
			if time.Now().Sub(mgr.purgeItemMap[tpltId]).Milliseconds() <
				definition.F_cache_purge_waiting_ms {
				continue
			}
			wg.Add(1)
			go func(id string) {
				mgr.pbh.PurgeTriplet(id)
				wg.Done()
			}(tpltId)
			delete(mgr.purgeItemMap, tpltId)
		}
		wg.Wait()
		mgr.pMtx.Unlock()
	}
}

func (mgr *CacheManager) dowloadAndWriteCache(
	fileName string, fid string) {
	exist, ossDataLen := CheckUrl(fileName)
	if !exist {
		return
	}

	// 1.Get from OSS
	ossData := mgr.DownLoad(fileName, ossDataLen)
	if ossData == nil {
		return
	}
	// 2. Write To Cache
	token, err := mgr.WriteToCache(fileName, definition.F_DB_STATE_READY, ossData)

	if err != nil {
		if strings.Contains(err.Error(), "cache full") {
			mgr.EnqueueDeletionReq()
			// TODO: rollback in background
			// mgr.RollbackFileInDB(pendingFid)
			return
		} else {
			log.Fatalln(err)
		}
	}
	log.Println("[dowloadAndWriteCache] token:", token)
	err = mgr.SealFileAtCache(fid, token, int32(len(ossData)))
	// TODO: if the error is conflict, return
	if err != nil {
		log.Fatalln(err)
	}
}

func (mgr *CacheManager) WriteToCache(
	fid string, state int32, ossData []byte) (string, error) {
	fw := file_handler.FileWriter{
		Pbh:    mgr.pbh,
		FileDb: mgr.dbOpsFile,
	}
	token := ""
	var err error
	if token, err = fw.WriteFileToCache(fid, ossData); err != nil {
		return "", err
	}
	return token, nil
}

func (mgr *CacheManager) SealFileAtCache(fid string, token string, size int32) error {
	err := mgr.dbOpsFile.CommitCacheFileInDB(
		fid, token, size)
	if err != nil {
		log.Printf("[ERROR] SealFileAtCache: Seal file(%s) failed.", fid)
		return err
	}
	return nil
}

// rollback file meta in db, if write cache failed
func (mgr *CacheManager) RollbackFileInDB(fid string) error {
	err := mgr.dbOpsFile.DeletePendingFileWithFIdInDB(fid)
	if err != nil {
		log.Printf("[ERROR] RollbackFileInDB: rollback file(%s) failed.", fid)
		return err
	}
	log.Printf("rollback with fid: %s", fid)
	return nil
}

// Utility function
func CheckUrl(arg string) (bool, int64) {
	//check url
	if definition.F_local_mode {
		stat, err := os.Stat(arg)
		if err == nil {
			return true, stat.Size()
		}
		if os.IsNotExist(err) {
			log.Printf("[CheckUrl] local file %v not found\n", arg)
			return false, 0
		}
		log.Println("[CheckUrl] error: ", err)
		return false, 0

	} else {
		resp, err := http.Head(arg)
		if err != nil {
			// maybe timeout , cannot crash the server.
			log.Println("[CheckUrl] error: ", err)
			return false, 0
		}
		log.Printf("[CheckUrl] check url =%v finish \n", arg)
		if resp.StatusCode == 404 {
			log.Printf("[CheckUrl] url: %s is not exist\n", arg)
			resp.Body.Close()
			return false, 0
		}
		contentlength := resp.ContentLength
		log.Printf("[CheckUrl] url: %s size: %v\n", arg, contentlength)
		if contentlength >= definition.F_CACHE_MAX_SIZE {
			log.Printf("[CheckUrl] url: %s is larger than %d MB\n", arg, definition.F_CACHE_MAX_SIZE/1024/1024)
			resp.Body.Close()
			return false, 0
		}
		resp.Body.Close()
		return true, contentlength
	}
}

// Utility function
func (mgr *CacheManager) DownLoad(url string, ossDataLen int64) []byte {
	// Get the data
	if definition.F_local_mode { // only for test
		data, error := ioutil.ReadFile(url)
		if error != nil {
			log.Println("read local file failed: ", error)
			return nil
		}
		if len(data) != int(ossDataLen) {
			log.Printf("[DownLoad] error: datalen %v is not equal to size %v\n", len(data), int(ossDataLen))
			return nil
		}
		return data

	} else {
		resp, err := http.Get(url)
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 404 {
			log.Println("[DownLoad] ossData not found")
			return nil
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, resp.Body)
		ossData := buf.Bytes()
		if err != nil {
			log.Println("[DownLoad] error: ", err)
			if len(ossData) != int(ossDataLen) {
				log.Printf("[DownLoad] error: datalen %v is not equal to size %v\n", len(ossData), int(ossDataLen))
				return nil
			}
		}
		log.Printf("[DownLoad] ossData len %v\n", len(ossData))
		return ossData
	}
}

// Utility function
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
