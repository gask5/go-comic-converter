package epub

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/disintegration/gift"
	"github.com/nwaples/rardecode"
	pdfimage "github.com/raff/pdfreader/image"
	"github.com/raff/pdfreader/pdfread"
	"golang.org/x/image/tiff"
)

type Image struct {
	Id     int
	Data   *ImageData
	Width  int
	Height int
}

type imageTask struct {
	Id     int
	Reader io.ReadCloser
}

func colorIsBlank(c color.Color) bool {
	g := color.GrayModel.Convert(c).(color.Gray)
	return g.Y >= 0xf0
}

func findMarging(img image.Image) image.Rectangle {
	imgArea := img.Bounds()

LEFT:
	for x := imgArea.Min.X; x < imgArea.Max.X; x++ {
		for y := imgArea.Min.Y; y < imgArea.Max.Y; y++ {
			if !colorIsBlank(img.At(x, y)) {
				break LEFT
			}
		}
		imgArea.Min.X++
	}

UP:
	for y := imgArea.Min.Y; y < imgArea.Max.Y; y++ {
		for x := imgArea.Min.X; x < imgArea.Max.X; x++ {
			if !colorIsBlank(img.At(x, y)) {
				break UP
			}
		}
		imgArea.Min.Y++
	}

RIGHT:
	for x := imgArea.Max.X - 1; x >= imgArea.Min.X; x-- {
		for y := imgArea.Min.Y; y < imgArea.Max.Y; y++ {
			if !colorIsBlank(img.At(x, y)) {
				break RIGHT
			}
		}
		imgArea.Max.X--
	}

BOTTOM:
	for y := imgArea.Max.Y - 1; y >= imgArea.Min.Y; y-- {
		for x := imgArea.Min.X; x < imgArea.Max.X; x++ {
			if !colorIsBlank(img.At(x, y)) {
				break BOTTOM
			}
		}
		imgArea.Max.Y--
	}

	return imgArea
}

func LoadImages(path string, options *ImageOptions) ([]*Image, error) {
	images := make([]*Image, 0)

	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	var (
		imageCount int
		imageInput chan *imageTask
	)

	if fi.IsDir() {
		imageCount, imageInput, err = loadDir(path)
	} else {
		switch ext := strings.ToLower(filepath.Ext(path)); ext {
		case ".cbz", ".zip":
			imageCount, imageInput, err = loadCbz(path)
		case ".cbr", ".rar":
			imageCount, imageInput, err = loadCbr(path)
		case ".pdf":
			imageCount, imageInput, err = loadPdf(path)
		default:
			err = fmt.Errorf("unknown file format (%s): support .cbz, .zip, .cbr, .rar, .pdf", ext)
		}
	}
	if err != nil {
		return nil, err
	}

	imageOutput := make(chan *Image)

	// processing
	wg := &sync.WaitGroup{}

	for i := 0; i < options.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for img := range imageInput {
				// Decode image
				src, _, err := image.Decode(img.Reader)
				if err != nil {
					panic(err)
				}

				if options.Crop {
					g := gift.New(gift.Crop(findMarging(src)))
					newSrc := image.NewNRGBA(g.Bounds(src.Bounds()))
					g.Draw(newSrc, src)
					src = newSrc
				}

				g := NewGift(options)

				// Convert image
				dst := image.NewPaletted(g.Bounds(src.Bounds()), options.Palette)
				if dst.Bounds().Dx() == 0 || dst.Bounds().Dy() == 0 {
					dst = image.NewPaletted(image.Rectangle{
						Min: image.Point{0, 0},
						Max: image.Point{1, 1},
					}, dst.Palette)
					dst.Set(0, 0, color.White)
				} else {
					g.Draw(dst, src)
				}

				// Encode image
				b := bytes.NewBuffer([]byte{})
				err = jpeg.Encode(b, dst, &jpeg.Options{Quality: options.Quality})
				if err != nil {
					panic(err)
				}

				name := fmt.Sprintf("OEBPS/Images/%d.jpg", img.Id)
				if img.Id == 0 {
					name = "OEBPS/Images/cover.jpg"
				}
				imageOutput <- &Image{
					img.Id,
					newImageData(name, b.Bytes()),
					dst.Bounds().Dx(),
					dst.Bounds().Dy(),
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(imageOutput)
	}()

	bar := NewBar(imageCount, "Processing", 1, 2)
	for image := range imageOutput {
		images = append(images, image)
		bar.Add(1)
	}
	bar.Close()

	if len(images) == 0 {
		return nil, fmt.Errorf("image not found")
	}

	sort.Slice(images, func(i, j int) bool {
		return images[i].Id < images[j].Id
	})

	return images, nil
}

func loadDir(input string) (int, chan *imageTask, error) {

	images := make([]string, 0)
	err := filepath.WalkDir(input, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if strings.ToLower(ext) != ".jpg" {
			return nil
		}

		images = append(images, path)
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(images) == 0 {
		return 0, nil, fmt.Errorf("image not found")
	}

	sort.Strings(images)

	output := make(chan *imageTask)
	go func() {
		defer close(output)
		for i, img := range images {
			f, err := os.Open(img)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			output <- &imageTask{
				Id:     i,
				Reader: f,
			}
		}
	}()
	return len(images), output, nil
}

func loadCbz(input string) (int, chan *imageTask, error) {
	r, err := zip.OpenReader(input)
	if err != nil {
		return 0, nil, err
	}

	images := make([]*zip.File, 0)
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if strings.ToLower(filepath.Ext(f.Name)) != ".jpg" {
			continue
		}
		images = append(images, f)
	}
	if len(images) == 0 {
		r.Close()
		return 0, nil, fmt.Errorf("no images found")
	}

	sort.SliceStable(images, func(i, j int) bool {
		return strings.Compare(images[i].Name, images[j].Name) < 0
	})

	output := make(chan *imageTask)
	go func() {
		defer close(output)
		for i, img := range images {
			f, err := img.Open()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			output <- &imageTask{
				Id:     i,
				Reader: f,
			}
		}
	}()
	return len(images), output, nil
}

func loadCbr(input string) (int, chan *imageTask, error) {
	// listing and indexing
	rl, err := rardecode.OpenReader(input, "")
	if err != nil {
		return 0, nil, err
	}
	names := make([]string, 0)
	for {
		f, err := rl.Next()

		if err != nil && err != io.EOF {
			rl.Close()
			return 0, nil, err
		}

		if f == nil {
			break
		}

		if f.IsDir {
			continue
		}

		if strings.ToLower(filepath.Ext(f.Name)) != ".jpg" {
			continue
		}

		names = append(names, f.Name)
	}
	rl.Close()

	if len(names) == 0 {
		return 0, nil, fmt.Errorf("no images found")
	}

	sort.Strings(names)

	indexedNames := make(map[string]int)
	for i, name := range names {
		indexedNames[name] = i
	}

	// send file to the queue
	output := make(chan *imageTask)
	go func() {
		defer close(output)
		r, err := rardecode.OpenReader(input, "")
		if err != nil {
			panic(err)
		}
		defer r.Close()

		for {
			f, err := r.Next()
			if err != nil && err != io.EOF {
				panic(err)
			}
			if f == nil {
				break
			}
			if idx, ok := indexedNames[f.Name]; ok {
				b := bytes.NewBuffer([]byte{})
				io.Copy(b, r)

				output <- &imageTask{
					Id:     idx,
					Reader: io.NopCloser(b),
				}
			}
		}
	}()

	return len(names), output, nil
}

func loadPdf(input string) (int, chan *imageTask, error) {
	pdf := pdfread.Load(input)
	if pdf == nil {
		return 0, nil, fmt.Errorf("can't read pdf")
	}

	nbPages := len(pdf.Pages())
	output := make(chan *imageTask)
	go func() {
		defer close(output)
		defer pdf.Close()
		for i := 0; i < nbPages; i++ {
			img, err := pdfimage.Extract(pdf, i+1)
			if err != nil {
				panic(err)
			}

			b := bytes.NewBuffer([]byte{})
			err = tiff.Encode(b, img, nil)
			if err != nil {
				panic(err)
			}

			output <- &imageTask{
				Id:     i,
				Reader: io.NopCloser(b),
			}
		}
	}()

	return nbPages, output, nil
}
