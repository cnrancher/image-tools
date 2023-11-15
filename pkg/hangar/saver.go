package hangar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"github.com/cnrancher/hangar/pkg/destination"
	"github.com/cnrancher/hangar/pkg/hangar/archive"
	"github.com/cnrancher/hangar/pkg/source"
	"github.com/cnrancher/hangar/pkg/types"
	"github.com/cnrancher/hangar/pkg/utils"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

// saveObject is the object for sending to worker pool when saving image
type saveObject struct {
	image       string
	source      *source.Source
	destination *destination.Destination
	timeout     time.Duration
	id          int
}

type Saver struct {
	*common

	aw        *archive.Writer
	awMutex   *sync.RWMutex
	index     *archive.Index
	layersSet map[digest.Digest]bool

	// Override the registry of source image to be copied
	SourceRegistry string
	// Override the project of source image to be copied
	SourceProject string
	// SharedBlobDirPath is the directory to save the shared blobs
	SharedBlobDirPath string
	// ArchiveName is the saved archive file name
	ArchiveName string
}

type SaverOpts struct {
	CommonOpts

	// Override the registry of source image to be copied
	SourceRegistry string
	// Override the project of source image to be copied
	SourceProject string
	// SharedBlobDirPath is the directory to save the shared blobs
	SharedBlobDirPath string
	// ArchiveName is the saved archive file name
	ArchiveName string
}

func NewSaver(o *SaverOpts) (*Saver, error) {
	s := &Saver{
		awMutex:   &sync.RWMutex{},
		index:     archive.NewIndex(),
		layersSet: make(map[digest.Digest]bool),

		SourceRegistry:    o.SourceRegistry,
		SourceProject:     o.SourceProject,
		SharedBlobDirPath: o.SharedBlobDirPath,
		ArchiveName:       o.ArchiveName,
	}
	if s.SharedBlobDirPath == "" {
		s.SharedBlobDirPath = archive.SharedBlobDir
	}
	var err error
	s.common, err = newCommon(&o.CommonOpts)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Saver) copy(ctx context.Context) {
	s.common.initErrorHandler(ctx)
	s.common.initWorker(ctx, s.worker)
	for i, img := range s.common.images {
		object := &saveObject{
			id:    i + 1,
			image: img,
		}
		sourceRegistry := utils.GetRegistryName(img)
		if s.SourceRegistry != "" {
			sourceRegistry = s.SourceRegistry
		}
		sourceProject := utils.GetProjectName(img)
		if s.SourceProject != "" {
			sourceProject = s.SourceProject
		}
		src, err := source.NewSource(&source.Option{
			Type:          types.TypeDocker,
			Registry:      sourceRegistry,
			Project:       sourceProject,
			Name:          utils.GetImageName(img),
			Tag:           utils.GetImageTag(img),
			SystemContext: s.systemContext,
		})
		if err != nil {
			s.handleError(fmt.Errorf("failed to init source image: %w", err))
			s.recordFailedImage(img)
			continue
		}
		object.source = src

		cd, err := s.newSaveCacheDir()
		if err != nil {
			s.handleError(fmt.Errorf("failed to create cache dir: %w", err))
			os.RemoveAll(cd)
			s.recordFailedImage(img)
			continue
		}
		sd := path.Join(cd, s.SharedBlobDirPath)
		dest, err := destination.NewDestination(&destination.Option{
			Type:          types.TypeOci,
			Directory:     cd,
			Name:          utils.GetImageName(img),
			Tag:           utils.GetImageTag(img),
			SystemContext: utils.SystemContextWithSharedBlobDir(s.systemContext, sd),
		})
		if err != nil {
			s.handleError(fmt.Errorf("failed to init dest image: %w", err))
			os.RemoveAll(cd)
			s.recordFailedImage(img)
			continue
		}
		object.destination = dest
		if err = s.handleObject(object); err != nil {
			os.RemoveAll(cd)
		}
	}
	s.waitWorkers()
	if err := s.writeIndex(); err != nil {
		logrus.Errorf("failed to write index file: %v", err)
	}
	if err := s.aw.Close(); err != nil {
		logrus.Errorf("failed to close archive writer: %v", err)
	}
}

func (s *Saver) newSaveCacheDir() (string, error) {
	cd, err := os.MkdirTemp(archive.CacheDir(), "*")
	if err != nil {
		return "", fmt.Errorf("os.MkdirTemp: %w", err)
	}
	logrus.Debugf("create save cache dir: %v", cd)
	return cd, nil
}

func (s *Saver) writeIndex() error {
	return s.aw.WriteIndex(s.index)
}

// Run save images from registry server into local directory / hangar archive.
func (s *Saver) Run(ctx context.Context) error {
	// Init Archive Writer.
	aw, err := archive.NewWriter(s.ArchiveName)
	if err != nil {
		return fmt.Errorf("failed to create archive %q: %w", s.ArchiveName, err)
	}
	s.aw = aw

	s.copy(ctx)
	if len(s.failedImageSet) != 0 {
		logrus.Errorf("Failed image list:")
		for i := range s.failedImageSet {
			fmt.Printf("%v\n", i)
		}
		return fmt.Errorf("some images failed to save")
	}
	return nil
}

func (s *Saver) worker(ctx context.Context, o any) {
	if o == nil {
		return
	}
	obj, ok := o.(*saveObject)
	if !ok {
		logrus.Errorf("skip object type(%T), data %v", o, o)
		return
	}

	var (
		copyContext context.Context
		cancel      context.CancelFunc
		err         error
	)
	if obj.timeout > 0 {
		copyContext, cancel = context.WithTimeout(ctx, obj.timeout)
	} else {
		copyContext, cancel = context.WithCancel(ctx)
	}
	defer func() {
		if err != nil {
			s.handleError(NewError(obj.id, err, obj.source, obj.destination))
			s.recordFailedImage(obj.image)
		}
		cancel()
		// Delete cache dir.
		if err = os.RemoveAll(obj.destination.Directory()); err != nil {
			logrus.Errorf("failed to delete cache dir %q: %v",
				obj.destination.Directory(), err)
		}
	}()

	err = obj.source.Init(copyContext)
	if err != nil {
		err = fmt.Errorf("failed to init source: %w", err)
		return
	}
	logrus.WithFields(logrus.Fields{"IMG": obj.id}).
		Infof("Saving [%v]", obj.source.ReferenceNameWithoutTransport())
	err = obj.destination.Init(copyContext)
	if err != nil {
		err = fmt.Errorf("failed to init destination: %w", err)
		return
	}
	err = obj.source.Copy(copyContext, obj.destination, s.imageSpecSet, s.policy)
	if err != nil {
		if errors.Is(err, utils.ErrNoAvailableImage) {
			logrus.WithFields(logrus.Fields{"IMG": obj.id}).
				Warnf("Skip save image [%v]: %v",
					obj.source.ReferenceNameWithoutTransport(), err)
			err = nil
		} else {
			err = fmt.Errorf("failed to copy [%v] to [%v]: %w",
				obj.source.ReferenceName(), obj.destination.ReferenceName(), err)
			return
		}
	}

	// images copied to cache folder, write to archive file
	s.awMutex.Lock()
	defer s.awMutex.Unlock()

	logrus.WithFields(logrus.Fields{"IMG": obj.id}).
		Debugf("Compressing [%v]", obj.destination.ReferenceNameWithoutTransport())

	shareBlobsDir := obj.destination.ReferenceNameWithoutTransport()
	copiedImage := obj.source.GetCopiedImage()
	filesToDelete := []string{}
	// Record image layers and remove duplicated layers.
	for _, image := range copiedImage.Images {
		for _, layer := range image.Layers {
			if s.layersSet[layer] {
				d := path.Join(shareBlobsDir, archive.SharedBlobDir,
					string(layer.Algorithm()), layer.Encoded())
				filesToDelete = append(filesToDelete, d)
			} else {
				s.layersSet[layer] = true
			}
		}
		if s.layersSet[image.Digest] {
			// The image already exists in archive, delete all resources.
			d1 := path.Join(shareBlobsDir, archive.SharedBlobDir,
				string(image.Digest.Algorithm()), image.Digest.Encoded())
			d2 := path.Join(shareBlobsDir, image.Digest.Encoded())
			filesToDelete = append(filesToDelete, d1)
			filesToDelete = append(filesToDelete, d2)
		} else {
			s.layersSet[image.Digest] = true
		}
		if image.Config != "" && s.layersSet[image.Config] {
			d := path.Join(shareBlobsDir, archive.SharedBlobDir,
				string(image.Config.Algorithm()), image.Config.Encoded())
			filesToDelete = append(filesToDelete, d)
		} else if image.Config != "" {
			s.layersSet[image.Config] = true
		}
	}
	for _, dir := range filesToDelete {
		if _, err := os.Stat(dir); err != nil {
			logrus.Debugf("failed to clean duplicated file %q: stat: %v",
				dir, err)
		}
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("failed to clean duplicated file %q: remove all: %v",
				dir, err)
		}
	}

	err = s.aw.Write(obj.destination.ReferenceNameWithoutTransport())
	if err != nil {
		err = fmt.Errorf("failed to write [%v] to [%v]: %w",
			obj.destination.ReferenceNameWithoutTransport(), s.ArchiveName, err)
		return
	}
	s.index.Append(copiedImage)
}

func (s *Saver) Validate(ctx context.Context) error {
	ar, err := archive.NewReader(s.ArchiveName)
	if err != nil {
		return fmt.Errorf("failed to create archive reader: %w", err)
	}
	b, err := ar.Index()
	if err != nil {
		return fmt.Errorf("failed to read archive index: %w", err)
	}
	if err := ar.Close(); err != nil {
		logrus.Errorf("failed to close archive reader: %v", err)
	}
	if err := s.index.Unmarshal(b); err != nil {
		return fmt.Errorf("failed to read archive index: %w", err)
	}
	s.validate(ctx)

	if len(s.failedImageSet) != 0 {
		logrus.Errorf("Validate failed image list:")
		for i := range s.failedImageSet {
			fmt.Printf("%v\n", i)
		}
		return fmt.Errorf("some images failed to validate")
	}
	return nil
}

func (s *Saver) validate(ctx context.Context) {
	s.common.initErrorHandler(ctx)
	s.common.initWorker(ctx, s.validateWorker)
	for i, img := range s.common.images {
		object := &saveObject{
			id:    i + 1,
			image: img,
		}
		sourceRegistry := utils.GetRegistryName(img)
		if s.SourceRegistry != "" {
			sourceRegistry = s.SourceRegistry
		}
		sourceProject := utils.GetProjectName(img)
		if s.SourceProject != "" {
			sourceProject = s.SourceProject
		}
		src, err := source.NewSource(&source.Option{
			Type:          types.TypeDocker,
			Registry:      sourceRegistry,
			Project:       sourceProject,
			Name:          utils.GetImageName(img),
			Tag:           utils.GetImageTag(img),
			SystemContext: s.systemContext,
		})
		if err != nil {
			s.handleError(fmt.Errorf("failed to init source image: %w", err))
			s.recordFailedImage(img)
			continue
		}
		object.source = src
		s.handleObject(object)
	}
	s.waitWorkers()
}

func (s *Saver) validateWorker(ctx context.Context, o any) {
	if o == nil {
		return
	}
	obj, ok := o.(*saveObject)
	if !ok {
		logrus.Errorf("skip object type(%T), data %v", o, o)
		return
	}

	var (
		validateContext context.Context
		cancel          context.CancelFunc
		err             error
	)
	if obj.timeout > 0 {
		validateContext, cancel = context.WithTimeout(ctx, obj.timeout)
	} else {
		validateContext, cancel = context.WithCancel(ctx)
	}

	defer func() {
		cancel()
		if err != nil {
			s.handleError(NewError(obj.id, err, nil, nil))
			s.recordFailedImage(obj.image)
		}
	}()

	err = obj.source.Init(validateContext)
	if err != nil {
		return
	}
	image := obj.source.ImageBySet(s.imageSpecSet)
	if !s.index.Has(image) {
		logrus.WithFields(logrus.Fields{"IMG": obj.id}).
			Errorf("Image [%v] does not exists in archive index",
				obj.source.ReferenceNameWithoutTransport())
		err = fmt.Errorf("FAILED: [%v]", obj.source.ReferenceNameWithoutTransport())
		return
	}
	logrus.Infof("PASS: [%v]", obj.source.ReferenceNameWithoutTransport())
}
