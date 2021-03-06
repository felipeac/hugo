// Copyright 2017-present The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resource

import (
	"errors"
	"fmt"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mitchellh/mapstructure"

	"github.com/gohugoio/hugo/helpers"
	"github.com/spf13/afero"

	// Importing image codecs for image.DecodeConfig
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"

	"github.com/disintegration/imaging"

	// Import webp codec
	"sync"

	_ "golang.org/x/image/webp"
)

var (
	_ Resource = (*Image)(nil)
	_ Source   = (*Image)(nil)
	_ Cloner   = (*Image)(nil)
)

// Imaging contains default image processing configuration. This will be fetched
// from site (or language) config.
type Imaging struct {
	// Default image quality setting (1-100). Only used for JPEG images.
	Quality int

	// Resample filter used. See https://github.com/disintegration/imaging
	ResampleFilter string
}

const (
	defaultJPEGQuality    = 75
	defaultResampleFilter = "box"
)

var imageFormats = map[string]imaging.Format{
	".jpg":  imaging.JPEG,
	".jpeg": imaging.JPEG,
	".png":  imaging.PNG,
	".tif":  imaging.TIFF,
	".tiff": imaging.TIFF,
	".bmp":  imaging.BMP,
	".gif":  imaging.GIF,
}

var anchorPositions = map[string]imaging.Anchor{
	strings.ToLower("Center"):      imaging.Center,
	strings.ToLower("TopLeft"):     imaging.TopLeft,
	strings.ToLower("Top"):         imaging.Top,
	strings.ToLower("TopRight"):    imaging.TopRight,
	strings.ToLower("Left"):        imaging.Left,
	strings.ToLower("Right"):       imaging.Right,
	strings.ToLower("BottomLeft"):  imaging.BottomLeft,
	strings.ToLower("Bottom"):      imaging.Bottom,
	strings.ToLower("BottomRight"): imaging.BottomRight,
}

var imageFilters = map[string]imaging.ResampleFilter{
	strings.ToLower("NearestNeighbor"):   imaging.NearestNeighbor,
	strings.ToLower("Box"):               imaging.Box,
	strings.ToLower("Linear"):            imaging.Linear,
	strings.ToLower("Hermite"):           imaging.Hermite,
	strings.ToLower("MitchellNetravali"): imaging.MitchellNetravali,
	strings.ToLower("CatmullRom"):        imaging.CatmullRom,
	strings.ToLower("BSpline"):           imaging.BSpline,
	strings.ToLower("Gaussian"):          imaging.Gaussian,
	strings.ToLower("Lanczos"):           imaging.Lanczos,
	strings.ToLower("Hann"):              imaging.Hann,
	strings.ToLower("Hamming"):           imaging.Hamming,
	strings.ToLower("Blackman"):          imaging.Blackman,
	strings.ToLower("Bartlett"):          imaging.Bartlett,
	strings.ToLower("Welch"):             imaging.Welch,
	strings.ToLower("Cosine"):            imaging.Cosine,
}

type Image struct {
	config       image.Config
	configInit   sync.Once
	configLoaded bool

	copiedToDestinationInit sync.Once

	imaging *Imaging

	hash string

	*genericResource
}

func (i *Image) Width() int {
	i.initConfig()
	return i.config.Width
}

func (i *Image) Height() int {
	i.initConfig()
	return i.config.Height
}

// Implement the Cloner interface.
func (i *Image) WithNewBase(base string) Resource {
	return &Image{
		imaging:         i.imaging,
		hash:            i.hash,
		genericResource: i.genericResource.WithNewBase(base).(*genericResource)}
}

// Resize resizes the image to the specified width and height using the specified resampling
// filter and returns the transformed image. If one of width or height is 0, the image aspect
// ratio is preserved.
func (i *Image) Resize(spec string) (*Image, error) {
	return i.doWithImageConfig("resize", spec, func(src image.Image, conf imageConfig) (image.Image, error) {
		return imaging.Resize(src, conf.Width, conf.Height, conf.Filter), nil
	})
}

// Fit scales down the image using the specified resample filter to fit the specified
// maximum width and height.
func (i *Image) Fit(spec string) (*Image, error) {
	return i.doWithImageConfig("fit", spec, func(src image.Image, conf imageConfig) (image.Image, error) {
		return imaging.Fit(src, conf.Width, conf.Height, conf.Filter), nil
	})
}

// Fill scales the image to the smallest possible size that will cover the specified dimensions,
// crops the resized image to the specified dimensions using the given anchor point.
// Space delimited config: 200x300 TopLeft
func (i *Image) Fill(spec string) (*Image, error) {
	return i.doWithImageConfig("fill", spec, func(src image.Image, conf imageConfig) (image.Image, error) {
		return imaging.Fill(src, conf.Width, conf.Height, conf.Anchor, conf.Filter), nil
	})
}

// Holds configuration to create a new image from an existing one, resize etc.
type imageConfig struct {
	Action string

	// Quality ranges from 1 to 100 inclusive, higher is better.
	// This is only relevant for JPEG images.
	// Default is 75.
	Quality int

	// Rotate rotates an image by the given angle counter-clockwise.
	// The rotation will be performed first.
	Rotate int

	Width  int
	Height int

	Filter    imaging.ResampleFilter
	FilterStr string

	Anchor    imaging.Anchor
	AnchorStr string
}

func (i *Image) isJPEG() bool {
	name := strings.ToLower(i.rel)
	return strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg")
}

func (i *Image) doWithImageConfig(action, spec string, f func(src image.Image, conf imageConfig) (image.Image, error)) (*Image, error) {
	conf, err := parseImageConfig(spec)
	if err != nil {
		return nil, err
	}
	conf.Action = action

	if conf.Quality <= 0 && i.isJPEG() {
		// We need a quality setting for all JPEGs
		conf.Quality = i.imaging.Quality
	}

	if conf.FilterStr == "" {
		conf.FilterStr = i.imaging.ResampleFilter
		conf.Filter = imageFilters[conf.FilterStr]
	}

	key := i.relPermalinkForRel(i.filenameFromConfig(conf))

	return i.spec.imageCache.getOrCreate(i.spec, key, func(resourceCacheFilename string) (*Image, error) {
		ci := i.clone()

		ci.setBasePath(conf)

		src, err := i.decodeSource()
		if err != nil {
			return nil, err
		}

		if conf.Rotate != 0 {
			// Rotate it befor any scaling to get the dimensions correct.
			src = imaging.Rotate(src, float64(conf.Rotate), color.Transparent)
		}

		converted, err := f(src, conf)
		if err != nil {
			return ci, err
		}

		b := converted.Bounds()
		ci.config = image.Config{Width: b.Max.X, Height: b.Max.Y}
		ci.configLoaded = true

		return ci, i.encodeToDestinations(converted, conf, resourceCacheFilename, ci.RelPermalink())
	})

}

func (i imageConfig) key() string {
	k := strconv.Itoa(i.Width) + "x" + strconv.Itoa(i.Height)
	if i.Action != "" {
		k += "_" + i.Action
	}
	if i.Quality > 0 {
		k += "_q" + strconv.Itoa(i.Quality)
	}
	if i.Rotate != 0 {
		k += "_r" + strconv.Itoa(i.Rotate)
	}
	k += "_" + i.FilterStr + "_" + i.AnchorStr
	return k
}

var defaultImageConfig = imageConfig{
	Action:    "",
	Anchor:    imaging.Center,
	AnchorStr: strings.ToLower("Center"),
}

func newImageConfig(width, height, quality, rotate int, filter, anchor string) imageConfig {
	c := defaultImageConfig

	c.Width = width
	c.Height = height
	c.Quality = quality
	c.Rotate = rotate

	if filter != "" {
		filter = strings.ToLower(filter)
		if v, ok := imageFilters[filter]; ok {
			c.Filter = v
			c.FilterStr = filter
		}
	}

	if anchor != "" {
		anchor = strings.ToLower(anchor)
		if v, ok := anchorPositions[anchor]; ok {
			c.Anchor = v
			c.AnchorStr = anchor
		}
	}

	return c
}

func parseImageConfig(config string) (imageConfig, error) {
	var (
		c   = defaultImageConfig
		err error
	)

	if config == "" {
		return c, errors.New("image config cannot be empty")
	}

	parts := strings.Fields(config)
	for _, part := range parts {
		part = strings.ToLower(part)

		if pos, ok := anchorPositions[part]; ok {
			c.Anchor = pos
			c.AnchorStr = part
		} else if filter, ok := imageFilters[part]; ok {
			c.Filter = filter
			c.FilterStr = part
		} else if part[0] == 'q' {
			c.Quality, err = strconv.Atoi(part[1:])
			if err != nil {
				return c, err
			}
			if c.Quality < 1 && c.Quality > 100 {
				return c, errors.New("quality ranges from 1 to 100 inclusive")
			}
		} else if part[0] == 'r' {
			c.Rotate, err = strconv.Atoi(part[1:])
			if err != nil {
				return c, err
			}
		} else if strings.Contains(part, "x") {
			widthHeight := strings.Split(part, "x")
			if len(widthHeight) <= 2 {
				first := widthHeight[0]
				if first != "" {
					c.Width, err = strconv.Atoi(first)
					if err != nil {
						return c, err
					}
				}

				if len(widthHeight) == 2 {
					second := widthHeight[1]
					if second != "" {
						c.Height, err = strconv.Atoi(second)
						if err != nil {
							return c, err
						}
					}
				}
			} else {
				return c, errors.New("invalid image dimensions")
			}

		}
	}

	if c.Width == 0 && c.Height == 0 {
		return c, errors.New("must provide Width or Height")
	}

	return c, nil
}

func (i *Image) initConfig() error {
	var err error
	i.configInit.Do(func() {
		if i.configLoaded {
			return
		}

		var (
			f      afero.File
			config image.Config
		)

		f, err = i.spec.Fs.Source.Open(i.AbsSourceFilename())
		if err != nil {
			return
		}
		defer f.Close()

		config, _, err = image.DecodeConfig(f)
		if err != nil {
			return
		}
		i.config = config
	})

	return err
}

func (i *Image) decodeSource() (image.Image, error) {
	file, err := i.spec.Fs.Source.Open(i.AbsSourceFilename())
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return imaging.Decode(file)
}

func (i *Image) copyToDestination(src string) error {
	var res error

	i.copiedToDestinationInit.Do(func() {
		target := filepath.Join(i.absPublishDir, i.RelPermalink())

		// Fast path:
		// This is a processed version of the original.
		// If it exists on destination with the same filename and file size, it is
		// the same file, so no need to transfer it again.
		if fi, err := i.spec.Fs.Destination.Stat(target); err == nil && fi.Size() == i.osFileInfo.Size() {
			return
		}

		in, err := i.spec.Fs.Source.Open(src)
		if err != nil {
			res = err
			return
		}
		defer in.Close()

		out, err := i.spec.Fs.Destination.Create(target)
		if err != nil {
			res = err
			return
		}
		defer out.Close()

		_, err = io.Copy(out, in)
		if err != nil {
			res = err
			return
		}
	})

	return res
}

func (i *Image) encodeToDestinations(img image.Image, conf imageConfig, resourceCacheFilename, filename string) error {
	ext := strings.ToLower(helpers.Ext(filename))

	imgFormat, ok := imageFormats[ext]
	if !ok {
		return imaging.ErrUnsupportedFormat
	}

	target := filepath.Join(i.absPublishDir, filename)

	file1, err := i.spec.Fs.Destination.Create(target)
	if err != nil {
		return err
	}
	defer file1.Close()

	var w io.Writer

	if resourceCacheFilename != "" {
		// Also save it to the image resource cache for later reuse.
		if err = i.spec.Fs.Source.MkdirAll(filepath.Dir(resourceCacheFilename), os.FileMode(0755)); err != nil {
			return err
		}

		file2, err := i.spec.Fs.Source.Create(resourceCacheFilename)
		if err != nil {
			return err
		}

		w = io.MultiWriter(file1, file2)
		defer file2.Close()
	} else {
		w = file1
	}

	switch imgFormat {
	case imaging.JPEG:

		var rgba *image.RGBA
		quality := conf.Quality

		if nrgba, ok := img.(*image.NRGBA); ok {
			if nrgba.Opaque() {
				rgba = &image.RGBA{
					Pix:    nrgba.Pix,
					Stride: nrgba.Stride,
					Rect:   nrgba.Rect,
				}
			}
		}
		if rgba != nil {
			return jpeg.Encode(w, rgba, &jpeg.Options{Quality: quality})
		} else {
			return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
		}
	default:
		return imaging.Encode(w, img, imgFormat)
	}

}

func (i *Image) clone() *Image {
	g := *i.genericResource

	return &Image{
		imaging:         i.imaging,
		hash:            i.hash,
		genericResource: &g}
}

func (i *Image) setBasePath(conf imageConfig) {
	i.rel = i.filenameFromConfig(conf)
}

func (i *Image) filenameFromConfig(conf imageConfig) string {
	p1, p2 := helpers.FileAndExt(i.rel)
	idStr := fmt.Sprintf("_H%s_%d", i.hash, i.osFileInfo.Size())

	// Do not change for no good reason.
	const md5Threshold = 100

	key := conf.key()

	// It is useful to have the key in clear text, but when nesting transforms, it
	// can easily be too long to read, and maybe even too long
	// for the different OSes to handle.
	if len(p1)+len(idStr)+len(p2) > md5Threshold {
		key = helpers.MD5String(p1 + key + p2)
		p1 = p1[:strings.Index(p1, "_H")]
	} else if strings.Contains(p1, idStr) {
		// On scaling an already scaled image, we get the file info from the original.
		// Repeating the same info in the filename makes it stuttery for no good reason.
		idStr = ""
	}

	return fmt.Sprintf("%s%s_%s%s", p1, idStr, key, p2)
}

func decodeImaging(m map[string]interface{}) (Imaging, error) {
	var i Imaging
	if err := mapstructure.WeakDecode(m, &i); err != nil {
		return i, err
	}

	if i.Quality <= 0 || i.Quality > 100 {
		i.Quality = defaultJPEGQuality
	}

	if i.ResampleFilter == "" {
		i.ResampleFilter = defaultResampleFilter
	} else {
		filter := strings.ToLower(i.ResampleFilter)
		_, found := imageFilters[filter]
		if !found {
			return i, fmt.Errorf("%q is not a valid resample filter", filter)
		}
		i.ResampleFilter = filter
	}

	return i, nil
}
