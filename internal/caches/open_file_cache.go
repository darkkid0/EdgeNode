// Copyright 2022 Liuxiangchao iwind.liu@gmail.com. All rights reserved.

package caches

import (
	"github.com/TeaOSLab/EdgeNode/internal/goman"
	"github.com/TeaOSLab/EdgeNode/internal/utils/linkedlist"
	"github.com/fsnotify/fsnotify"
	"github.com/iwind/TeaGo/logs"
	"github.com/iwind/TeaGo/types"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type OpenFileCache struct {
	poolMap  map[string]*OpenFilePool // file path => Pool
	poolList *linkedlist.List
	watcher  *fsnotify.Watcher

	locker sync.RWMutex

	maxSize int
	count   int
}

func NewOpenFileCache(maxSize int) (*OpenFileCache, error) {
	if maxSize <= 0 {
		maxSize = 16384
	}

	var cache = &OpenFileCache{
		maxSize:  maxSize,
		poolMap:  map[string]*OpenFilePool{},
		poolList: linkedlist.NewList(),
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	cache.watcher = watcher

	goman.New(func() {
		for event := range watcher.Events {
			if runtime.GOOS == "linux" || event.Op&fsnotify.Chmod != fsnotify.Chmod {
				cache.Close(event.Name)
			}
		}
	})

	return cache, nil
}

func (this *OpenFileCache) Get(filename string) *OpenFile {
	this.locker.RLock()
	pool, ok := this.poolMap[filename]
	this.locker.RUnlock()
	if ok {
		file, consumed := pool.Get()
		if consumed {
			this.locker.Lock()
			this.count--

			// pool如果为空，也不需要从列表中删除，避免put时需要重新创建

			this.locker.Unlock()
		}
		return file
	}
	return nil
}

func (this *OpenFileCache) Put(filename string, file *OpenFile) {
	this.locker.Lock()
	defer this.locker.Unlock()

	pool, ok := this.poolMap[filename]
	var success bool
	if ok {
		success = pool.Put(file)
	} else {
		_ = this.watcher.Add(filename)
		pool = NewOpenFilePool(filename)
		pool.version = file.version
		this.poolMap[filename] = pool
		success = pool.Put(file)
	}
	this.poolList.Push(pool.linkItem)

	// 检查长度
	if success {
		this.count++

		// 如果超过当前容量，则关闭最早的
		if this.count > this.maxSize {
			var delta = this.maxSize / 100 // 清理1%
			if delta == 0 {
				delta = 1
			}
			for i := 0; i < delta; i++ {
				var head = this.poolList.Head()
				if head == nil {
					break
				}

				var headPool = head.Value.(*OpenFilePool)
				headFile, consumed := headPool.Get()
				if consumed {
					this.count--
					if headFile != nil {
						_ = headFile.Close()
					}
				}

				if headPool.Len() == 0 {
					delete(this.poolMap, headPool.filename)
					this.poolList.Remove(head)
					_ = this.watcher.Remove(headPool.filename)
				}
			}
		}
	}
}

func (this *OpenFileCache) Close(filename string) {
	this.locker.Lock()

	pool, ok := this.poolMap[filename]
	if ok {
		// 设置关闭状态
		pool.SetClosing()

		delete(this.poolMap, filename)
		this.poolList.Remove(pool.linkItem)
		_ = this.watcher.Remove(filename)
		this.count -= pool.Len()
	}

	this.locker.Unlock()

	// 在locker之外，提升性能
	if ok {
		pool.Close()
	}
}

func (this *OpenFileCache) CloseAll() {
	this.locker.Lock()
	for _, pool := range this.poolMap {
		pool.Close()
	}
	this.poolMap = map[string]*OpenFilePool{}
	this.poolList.Reset()
	_ = this.watcher.Close()
	this.count = 0
	this.locker.Unlock()
}

func (this *OpenFileCache) Debug() {
	var ticker = time.NewTicker(5 * time.Second)
	goman.New(func() {
		for range ticker.C {
			logs.Println("==== " + types.String(this.count) + " ====")
			this.poolList.Range(func(item *linkedlist.Item) (goNext bool) {
				logs.Println(filepath.Base(item.Value.(*OpenFilePool).Filename()), item.Value.(*OpenFilePool).Len())
				return true
			})
		}
	})
}
