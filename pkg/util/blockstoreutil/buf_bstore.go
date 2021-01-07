package blockstoreutil

import (
	"context"
	"os"

	block "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("bufbs")

type BufferedBS struct {
	read  Blockstore
	write Blockstore

	readviewer  Viewer
	writeviewer Viewer
}

func NewBufferedBstore(base Blockstore) *BufferedBS {
	var buf Blockstore
	if os.Getenv("LOTUS_DISABLE_VM_BUF") == "iknowitsabadidea" {
		log.Warn("VM BLOCKSTORE BUFFERING IS DISABLED")
		buf = base
	} else {
		buf = NewTemporary()
	}

	bs := &BufferedBS{
		read:  base,
		write: buf,
	}
	if v, ok := base.(Viewer); ok {
		bs.readviewer = v
	}
	if v, ok := buf.(Viewer); ok {
		bs.writeviewer = v
	}
	if (bs.writeviewer == nil) != (bs.readviewer == nil) {
		log.Warnf("one of the stores is not viewable; running less efficiently")
	}
	return bs
}

func NewTieredBstore(r Blockstore, w Blockstore) *BufferedBS {
	return &BufferedBS{
		read:  r,
		write: w,
	}
}

var _ Blockstore = (*BufferedBS)(nil)
var _ Viewer = (*BufferedBS)(nil)

func (bs *BufferedBS) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	a, err := bs.read.AllKeysChan(ctx)
	if err != nil {
		return nil, err
	}

	b, err := bs.write.AllKeysChan(ctx)
	if err != nil {
		return nil, err
	}

	out := make(chan cid.Cid)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			select {
			case val, ok := <-a:
				if !ok {
					a = nil
				} else {
					select {
					case out <- val:
					case <-ctx.Done():
						return
					}
				}
			case val, ok := <-b:
				if !ok {
					b = nil
				} else {
					select {
					case out <- val:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return out, nil
}

func (bs *BufferedBS) DeleteBlock(c cid.Cid) error {
	if err := bs.read.DeleteBlock(c); err != nil {
		return err
	}

	return bs.write.DeleteBlock(c)
}

func (bs *BufferedBS) View(c cid.Cid, callback func([]byte) error) error {
	if bs.writeviewer == nil || bs.readviewer == nil {
		// one of the stores isn't Viewer; fall back to pure Get behaviour.
		blk, err := bs.Get(c)
		if err != nil {
			return err
		}
		return callback(blk.RawData())
	}

	// both stores are viewable.
	if err := bs.writeviewer.View(c, callback); err == ErrNotFound {
		// not found in write blockstore; fall through.
	} else {
		return err // propagate errors, or nil, i.e. found.
	}
	return bs.readviewer.View(c, callback)
}

func (bs *BufferedBS) Get(c cid.Cid) (block.Block, error) {
	if out, err := bs.write.Get(c); err != nil {
		if err != ErrNotFound {
			return nil, err
		}
	} else {
		return out, nil
	}

	return bs.read.Get(c)
}

func (bs *BufferedBS) GetSize(c cid.Cid) (int, error) {
	s, err := bs.read.GetSize(c)
	if err == ErrNotFound || s == 0 {
		return bs.write.GetSize(c)
	}

	return s, err
}

func (bs *BufferedBS) Put(blk block.Block) error {
	has, err := bs.read.Has(blk.Cid()) // TODO: consider dropping this check
	if err != nil {
		return err
	}

	if has {
		return nil
	}

	return bs.write.Put(blk)
}

func (bs *BufferedBS) Has(c cid.Cid) (bool, error) {
	has, err := bs.write.Has(c)
	if err != nil {
		return false, err
	}
	if has {
		return true, nil
	}

	return bs.read.Has(c)
}

func (bs *BufferedBS) HashOnRead(hor bool) {
	bs.read.HashOnRead(hor)
	bs.write.HashOnRead(hor)
}

func (bs *BufferedBS) PutMany(blks []block.Block) error {
	return bs.write.PutMany(blks)
}

func (bs *BufferedBS) Read() Blockstore {
	return bs.read
}

func (bs *BufferedBS) Write() Blockstore {
	return bs.write
}
