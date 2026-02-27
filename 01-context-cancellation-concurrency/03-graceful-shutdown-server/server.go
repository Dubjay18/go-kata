package gracefulshutdownserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	*http.Server
	workers     chan func()
	ctxCancel   context.CancelFunc
	wg          sync.WaitGroup
	dbConns     []net.Conn
	workerCount int
}

func (s *Server) Start(ctx context.Context) error {
	for i := 0; i < s.workerCount; i++ {
		go func() {
			for {
				select {
				case job, ok := <-s.workers:
					if !ok {
						return
					}
					s.wg.Add(1)
					job()
					s.wg.Done()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// background cache warmer
	go func() {
		ticker := time.NewTicker(3 * 10 * time.Second)
		defer ticker.Stop()
		for {

			select {
			case <-ticker.C:
				fmt.Println("warming cache...")
			case <-ctx.Done():
				return
			}
		}
	}()
	errChan := make(chan error, 1)
	go func() {
		errChan <- s.ListenAndServe()
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return nil
	}

}

func (s *Server) Stop(ctx context.Context) error {
	if err := s.Shutdown(ctx); err != nil {
		return err
	}
	close(s.workers)
	s.wg.Wait()
	for _, db := range s.dbConns {
		db.Close()
	}
	return nil
}

func main() {
	ctx := context.Background()
	rootCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	s := &Server{
		Server:      &http.Server{Addr: ":8080"},
		workers:     make(chan func(), 4),
		workerCount: 4,
		dbConns:     []net.Conn{},
	}
	go s.Start(rootCtx)
	<-rootCtx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.Stop(stopCtx)
}
