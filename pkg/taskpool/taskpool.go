// Package taskpool 提供有界异步任务池，防止 goroutine 无限膨胀导致雪崩
package taskpool

import (
	"sync"

	"github.com/1239t/vohive/pkg/logger"
)

// Pool 有界异步任务池
type Pool struct {
	name string
	sem  chan struct{}
	wg   sync.WaitGroup
}

// New 创建一个具有指定并发上限的任务池
func New(name string, maxConcurrency int) *Pool {
	if maxConcurrency <= 0 {
		maxConcurrency = 16
	}
	return &Pool{
		name: name,
		sem:  make(chan struct{}, maxConcurrency),
	}
}

// Go 异步提交任务。如果池已满则丢弃并打印警告（非阻塞）
func (p *Pool) Go(fn func()) {
	select {
	case p.sem <- struct{}{}:
		p.wg.Add(1)
		go func() {
			defer func() {
				<-p.sem
				p.wg.Done()
			}()
			fn()
		}()
	default:
		logger.Warn("任务池已满，丢弃任务", "pool", p.name)
	}
}

// Wait 等待所有正在执行的任务完成（用于优雅关闭）
func (p *Pool) Wait() {
	p.wg.Wait()
}
