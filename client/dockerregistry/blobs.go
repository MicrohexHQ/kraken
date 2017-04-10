package dockerregistry

import (
	"io"
	"io/ioutil"
	"os"
	"sync"
	"time"

	"code.uber.internal/infra/kraken/client/store"
	"code.uber.internal/infra/kraken/client/torrentclient"
	"code.uber.internal/infra/kraken/kraken/test-tracker"

	"code.uber.internal/go-common.git/x/log"
	"github.com/anacrolix/torrent"
	sd "github.com/docker/distribution/registry/storage/driver"
)

// Blobs b
type Blobs struct {
	tracker *tracker.Tracker
	client  *torrentclient.Client
	store   *store.LocalFileStore
}

// NewBlobs creates Blobs
func NewBlobs(tr *tracker.Tracker, cl *torrentclient.Client, s *store.LocalFileStore) *Blobs {
	return &Blobs{
		tracker: tr,
		client:  cl,
		store:   s,
	}
}

func (b *Blobs) getBlobStat(fileName string) (sd.FileInfo, error) {
	info, err := b.store.GetCacheFileStat(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			err = b.download(fileName)
			if err != nil {
				return nil, sd.PathNotFoundError{
					DriverName: "kraken",
					Path:       fileName,
				}
			}
			info, err = b.store.GetCacheFileStat(fileName)
			if err != nil {
				return nil, err
			}
		}
	}

	fi := sd.FileInfoInternal{
		FileInfoFields: sd.FileInfoFields{
			Path:    info.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			IsDir:   info.IsDir(),
		},
	}
	return fi, nil
}

func (b *Blobs) download(fileName string) error {
	completed := false

	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		uri, err := b.tracker.GetMagnet(fileName)
		if err != nil {
			wg.Done()
			return
		}
		t, err := b.client.AddTorrentMagnet(uri)
		if err != nil {
			wg.Done()
			return
		}

		completedPieces := 0
		psc := t.SubscribePieceStateChanges()
		to := make(chan byte, 1)
		go func() {
			time.Sleep(downloadTimeout * time.Second)
			to <- uint8(1)
		}()
		go func() {
			for {
				select {
				case v := <-psc.Values:
					if v.(torrent.PieceStateChange).Complete {
						completedPieces = completedPieces + 1
					}
					if completedPieces == t.NumPieces() {
						completed = true
						wg.Done()
						return
					}
				case <-to:
					log.Errorf("Timeout waiting for %s to download", fileName)
					wg.Done()
					return
				}
			}
		}()
		log.Info("wait for info")
		<-t.GotInfo()
		log.Info("wait for download")
		t.DownloadAll()
	}()

	wg.Wait()
	if completed {
		return nil
	}

	return sd.PathNotFoundError{
		DriverName: "kraken",
		Path:       fileName,
	}
}

func (b *Blobs) getOrDownloadBlobData(fileName string) (data []byte, err error) {
	// check cache
	reader, err := b.getOrDownloadBlobReader(fileName, 0)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return ioutil.ReadAll(reader)
}

func (b *Blobs) getOrDownloadBlobReader(fileName string, offset int64) (reader io.ReadCloser, err error) {
	reader, err = b.getBlobReader(fileName, offset)
	if err != nil {
		if os.IsNotExist(err) {
			err = b.download(fileName)
			if err != nil {
				return nil, err
			}
			return b.getBlobReader(fileName, offset)
		}
		return nil, err
	}
	return reader, nil
}

func (b *Blobs) getBlobReader(fileName string, offset int64) (io.ReadCloser, error) {
	reader, err := b.store.GetCacheFileReader(fileName)
	if err != nil {
		return nil, err
	}

	// set offest
	_, err = reader.Seek(offset, 0)
	if err != nil {
		reader.Close()
		return nil, err
	}

	return reader, nil
}