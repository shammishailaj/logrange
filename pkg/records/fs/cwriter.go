package fs

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jrivets/log4g"
	"github.com/logrange/logrange/pkg/records"
)

type (
	// cWriter supports writing to a file-chunk. The implementation controls
	// fWriter lifecycle for accessing to the file. Only one goroutine can
	// write into the file at a time. So as the implementation uses fWriter,
	// which has a buffer, it tracks position of confirmed write (synced) records
	// positions to the file and unconfirmed (lro) last record, which is written
	// but not flushed to the sile yet. For to be read throug the file access,
	// any reader should consider lroCfrmd value as last record, because all other
	// ones can be not synced yet, so could be read inconsistent
	//
	// The cWriter has 2 timers - idle and flush. The idle timeout allows to
	// close underlying file descriptor (fWriter) if no write operation happens
	// in the timeout period. The flush timeout allows to flush buffer to the
	// disk in the period of time after last write if it is needed.
	cWrtier struct {
		lock sync.Mutex
		// confirmed last record offset (flushed)
		lroCfrmd int64
		// last raw record offset: unconfirmed(not flushed) record
		lro int64

		fileName  string
		w         *fWriter
		wSgnlChan chan bool

		// closed flag indicates wht cWriter is closed
		closed int32

		logger log4g.Logger

		// writer stuff
		sizeBuf []byte

		// idle timeout (to close the writer)
		idleTO time.Duration
		// flush timeout
		flushTO time.Duration
	}
)

func newCWriter(fileName string, lro int64) *cWrtier {
	cw := new(cWrtier)
	cw.fileName = fileName
	cw.lro = lro
	cw.lroCfrmd = lro
	cw.sizeBuf = make([]byte, ChnkDataHeaderSize)
	cw.idleTO = ChnkWriterIdleTO
	cw.flushTO = ChnkWriterFlushTO
	cw.logger = log4g.GetLogger("chunk.writer").WithId("{" + fileName + "}").(log4g.Logger)
	return cw
}

func (cw *cWrtier) ensureFWriter() error {
	if atomic.LoadInt32(&cw.closed) != 0 {
		return ErrWrongState
	}

	var err error
	if cw.w == nil {
		cw.w, err = newFWriter(cw.fileName, ChnkWriterBufSize)
		if err != nil {
			return err
		}

		// put 100 to be sure there is a buffer for not blocking signaling routine
		cw.wSgnlChan = make(chan bool, 100)

		go func(sc chan bool) {
			for {
				for !cw.isFlushNeeded() {
					lro := atomic.LoadInt64(&cw.lro)
					select {
					case <-time.After(cw.idleTO):
						// check whether lro was advanced while it was sleeping
						if atomic.LoadInt64(&cw.lro) == lro {
							cw.closeFWriter()
							return
						}
					case _, ok := <-sc:
						if !ok {
							// the channel closed
							return
						}
					}
				}

				select {
				case <-time.After(cw.flushTO):
					cw.flush()
				case _, ok := <-sc:
					if !ok {
						return
					}
				}
			}
		}(cw.wSgnlChan)
	}
	return nil
}

// isFlushNeeded returns whether the write buffer (see fWrtier) should be be
// flushed or not
func (cw *cWrtier) isFlushNeeded() bool {
	return atomic.LoadInt64(&cw.lroCfrmd) != atomic.LoadInt64(&cw.lro)
}

// write receives an iterator and writes records to the file-chunk.
//
// the write returns number of records written, offset for the last written
// record and an error if any. It can return an error together with non-zero
// first two parameters, which will indicate that some data was written.
//
// It will return no error if iterator is empty (the iterator returns io.EOF)
//
// The function holds lock, so it guarantees that only one go-routine can write into the
// chunk. Holding the lock is made from the performance prospective,
// so it checks whether the writer is closed after every record is written.
// See Close(), which sets the flag without requesting the lock.
//
// the write procedure happens in the context of ctx. Which is used for getting
// records from the iterator.
func (cw *cWrtier) write(ctx context.Context, it records.Iterator) (int, int64, error) {
	cw.lock.Lock()
	defer cw.lock.Unlock()
	err := cw.ensureFWriter()
	if err != nil {
		return 0, cw.lro, err
	}

	// indicates that flush signal already issued
	signal := cw.isFlushNeeded()
	clsd := false
	var wrtn int
	// checking the closed flag holding cw.lock, allows us to detect Close()
	// call and give up before we iterated completely over the iterator
	for atomic.LoadInt32(&cw.closed) == 0 {
		var rec records.Record
		rec, err = it.GetCtx(ctx)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}

		binary.BigEndian.PutUint32(cw.sizeBuf, uint32(len(rec)))
		var ro int64
		ro, err = cw.w.write(cw.sizeBuf)
		if err != nil {
			cw.logger.Error("Could not write a record header. err=", err)
			clsd = true
			break
		}

		_, err = cw.w.write(rec)
		if err != nil {
			// close chunk (unrecoverable error)
			cw.logger.Error("Could not write a record payload. err=", err)
			clsd = true
			break
		}

		_, err = cw.w.write(cw.sizeBuf)
		if err != nil {
			// close chunk (unrecoverable error)
			cw.logger.Error("Could not write a record footer. err=", err)
			clsd = true
			break
		}

		// update last raw record
		cw.lro = ro
		it.Next()
		wrtn++

	}

	if atomic.LoadInt32(&cw.closed) != 0 {
		err = ErrWrongState
	} else if clsd {
		cw.closeUnsafe()
	} else if cw.w.buffered() == 0 {
		// ok, write buffer is empty, no flush is needed
		cw.lroCfrmd = cw.lro
	} else if !signal {
		// signal the channel about write anyway
		cw.wSgnlChan <- true
	}

	return wrtn, cw.lro, err
}

func (cw *cWrtier) flush() {
	cw.lock.Lock()
	defer cw.lock.Unlock()

	if cw.w != nil {
		cw.w.flush()
	}
	cw.lroCfrmd = cw.lro
}

func (cw *cWrtier) closeFWriter() error {
	cw.lock.Lock()
	defer cw.lock.Unlock()

	return cw.closeFWriterUnsafe()
}

func (cw *cWrtier) closeFWriterUnsafe() error {
	var err error
	if cw.w != nil {
		err = cw.w.Close()
		cw.lroCfrmd = cw.lro
		cw.w = nil
		close(cw.wSgnlChan)
		cw.wSgnlChan = nil
	}
	return err
}

func (cw *cWrtier) Close() (err error) {
	atomic.StoreInt32(&cw.closed, 1)
	cw.lock.Lock()
	defer cw.lock.Unlock()

	cw.logger.Debug("Closing...")
	return cw.closeFWriterUnsafe()
}

func (cw *cWrtier) closeUnsafe() (err error) {
	cw.closed = 1
	return cw.closeFWriterUnsafe()
}