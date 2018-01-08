package arw

import (
	"bytes"
	"github.com/gonum/matrix/mat64"
	"image"
	"image/color"
	"io"
	"log"
	"reflect"
	"unsafe"
)

type rawDetails struct {
	width         uint16
	height        uint16
	bitDepth      uint16
	rawType       sonyRawFile
	offset        uint32
	stride        uint32
	length        uint32
	blackLevel    [4]uint16
	WhiteBalance  [4]int16
	gammaCurve    [5]uint16
	crop          image.Rectangle
	cfaPattern    [4]uint8 //TODO(sjon): This might not always be 4 bytes is my suspicion. We currently take from the offset
	cfaPatternDim [2]uint16
}

func extractDetails(rs io.ReadSeeker) (rawDetails, error) {
	var rw rawDetails

	header, err := ParseHeader(rs)
	meta, err := ExtractMetaData(rs, int64(header.Offset), 0)
	if err != nil {
		return rw, err
	}

	for _, fia := range meta.FIA {
		if fia.Tag == SubIFDs {
			rawIFD, err := ExtractMetaData(rs, int64(fia.Offset), 0)
			if err != nil {
				return rw, err
			}

			for i, v := range rawIFD.FIA {
				switch v.Tag {
				case ImageWidth:
					rw.width = uint16(v.Offset)
				case ImageHeight:
					rw.height = uint16(v.Offset)
				case BitsPerSample:
					rw.bitDepth = uint16(v.Offset)
				case SonyRawFileType:
					rw.rawType = sonyRawFile(v.Offset)
				case StripOffsets:
					rw.offset = v.Offset
				case RowsPerStrip:
					rw.stride = v.Offset //TODO(sjon): Uncompressed RAW files are 2 bytes per pixel whereas CRAW is 1 byte per pixel, this shouldn't be set here! current behaviour is for CRAW, add a divide by 2 for RAW
				case StripByteCounts:
					rw.length = v.Offset
				case SonyCurve:
					curve := *rawIFD.FIAvals[i].short
					copy(rw.gammaCurve[:4], curve)
					rw.gammaCurve[4] = 0x3fff
				case BlackLevel2:
					black := *rawIFD.FIAvals[i].short
					copy(rw.blackLevel[:], black)
				case WB_RGGBLevels:
					balance := *rawIFD.FIAvals[i].sshort
					copy(rw.WhiteBalance[:], balance)
				case DefaultCropSize:
				case CFAPattern2:
					rw.cfaPattern[0] = uint8((v.Offset & 0x000000ff) >> 0)
					rw.cfaPattern[1] = uint8((v.Offset & 0x0000ff00) >> 8)
					rw.cfaPattern[2] = uint8((v.Offset & 0x00ff0000) >> 16)
					rw.cfaPattern[3] = uint8((v.Offset & 0xff000000) >> 24)
				case CFARepeatPatternDim:
					rw.cfaPatternDim[0] = uint16((v.Offset * 0x0000ffff) >> 0)
					rw.cfaPatternDim[1] = uint16((v.Offset * 0xffff0000) >> 16)
				}
			}
		}

		if fia.Tag == DNGPrivateData {
			dng, err := ExtractMetaData(rs, int64(fia.Offset), 0)
			if err != nil {
				return rw, err
			}

			var sr2offset uint32
			var sr2length uint32
			var sr2key [4]byte

			for i := range dng.FIA {
				if dng.FIA[i].Tag == SR2SubIFDOffset {
					offset := dng.FIA[i].Offset
					sr2offset = offset
				}
				if dng.FIA[i].Tag == SR2SubIFDLength {
					sr2length = dng.FIA[i].Offset
				}
				if dng.FIA[i].Tag == SR2SubIFDKey {
					key := dng.FIA[i].Offset*0x0edd + 1
					sr2key[3] = byte((key >> 24) & 0xff)
					sr2key[2] = byte((key >> 16) & 0xff)
					sr2key[1] = byte((key >> 8) & 0xff)
					sr2key[0] = byte((key) & 0xff)
				}
			}

			buf := DecryptSR2(rs, sr2offset, sr2length)
			br := bytes.NewReader(buf)

			sr2, err := ExtractMetaData(br, 0, 0)
			if err != nil {
				log.Fatal(err)
			}

			for i, v := range sr2.FIA {
				switch v.Tag {
				case BlackLevel2:
					black := *sr2.FIAvals[i].short
					copy(rw.blackLevel[:], black)
				case WB_RGGBLevels:
					balance := *sr2.FIAvals[i].sshort
					copy(rw.WhiteBalance[:], balance)
				}
			}
		}
	}

	return rw, nil
}

//Helper function for gammacorrect
func vandermonde(a []float64, degree int) *mat64.Dense {
	x := mat64.NewDense(len(a), degree+1, nil)
	for i := range a {
		for j, p := 0, 1.; j <= degree; j, p = j+1, p*a[i] {
			x.Set(i, j, p)
		}
	}
	return x
}

//This function is created by gammacorrect
var xfactors [6]float64

func gamma(x float64) float64 {
	if x > 0x3fff {
		//panic("This shouldn't be happening!")
		return 0x3fff //TODO(sjon): Should it be concidered a bug if we receive blown out values here?
	}
	/*TODO(sjon): What should the correct value be here? Lower values seem to work for most inputs
	the building sample works ok with 0x200 but the baloon sample clips in multiple places with values lower than 0xcc
	*/
	x /= 0xCCC //We need to keep x in between 0 and 5, this maps to 0x0 to 0x3fff

	x5 := xfactors[5] * x * x * x * x * x
	x4 := xfactors[4] * x * x * x * x
	x3 := xfactors[3] * x * x * x
	x2 := xfactors[2] * x * x
	x1 := xfactors[1] * x
	x0 := xfactors[0] * 1
	val := x5 + x4 + x3 + x2 + x1 + x0 //The negative signs are already in the numbers
	return val
}

//The gamma curve points are in a 14 bit space space where we draw a curve that goes through the points.
func gammacorrect(curve [4]uint32) {
	x := []float64{0, 1, 2, 3, 4, 5} // It would be nice if we could make this [0,0x3fff] but that seems to be impossible

	y := []float64{0, float64(curve[0]), float64(curve[1]), float64(curve[2]), float64(curve[3]), 0x3fff}
	const degree = 5

	a := vandermonde(x, degree)
	b := mat64.NewDense(len(y), 1, y)
	c := mat64.NewDense(degree+1, 1, nil)

	qr := new(mat64.QR)
	qr.Factorize(a)

	if err := c.SolveQR(qr, false, b); err != nil {
		log.Println(err)
	}

	xfactors[5] = c.At(5, 0)
	xfactors[4] = c.At(4, 0)
	xfactors[3] = c.At(3, 0)
	xfactors[2] = c.At(2, 0)
	xfactors[1] = c.At(1, 0)
	xfactors[0] = c.At(0, 0)
}

func process(cur uint32, black uint32, whiteBalance float64) uint32 {
	const gammaspace = 1.596472423

	if cur <= black {
		return cur
	} else {
		cur -= black
	}

	balanced := float64(cur) * whiteBalance
	return uint32(gamma(balanced) * gammaspace)
}

func readCRAW(buf []byte, rw rawDetails) *RGB14 {
	img := NewRGB14(image.Rect(0, 0, int(rw.width), int(rw.height)))

	var gamma [4]uint32
	gamma[0] = uint32(rw.gammaCurve[0])
	gamma[1] = uint32(rw.gammaCurve[1])
	gamma[2] = uint32(rw.gammaCurve[2])
	gamma[3] = uint32(rw.gammaCurve[3])
	gammacorrect(gamma)

	var whiteBalanceRGGB [4]float64
	var maxBalance int16
	if rw.WhiteBalance[0] > rw.WhiteBalance[1] {
		maxBalance = rw.WhiteBalance[0]
	} else {
		maxBalance = rw.WhiteBalance[1]
	}
	if rw.WhiteBalance[2] > maxBalance {
		maxBalance = rw.WhiteBalance[2]
	}
	if rw.WhiteBalance[3] > maxBalance {
		maxBalance = rw.WhiteBalance[3]
	}

	whiteBalanceRGGB[0] = float64(rw.WhiteBalance[0]) / float64(maxBalance)
	whiteBalanceRGGB[1] = float64(rw.WhiteBalance[1]) / float64(maxBalance)
	whiteBalanceRGGB[2] = float64(rw.WhiteBalance[2]) / float64(maxBalance)
	whiteBalanceRGGB[3] = float64(rw.WhiteBalance[3]) / float64(maxBalance)

	log.Println(whiteBalanceRGGB)

	for y := 0; y < img.Rect.Max.Y; y++ {
		for x := 0; x < img.Rect.Max.X; x += 32 {
			if y%2 == 0 {
				base := y*img.Stride + x

				//fmt.Printf("Red block on line: %v\t column: %v\n", y, x)
				block := readCrawBlock(buf[base : base+pixelBlockSize]) //16 red pixels, inverleaved with following 16 green
				red := block.Decompress()

				//fmt.Printf("Green block on line: %v\t column: %v\n", y, x+pixelBlockSize)
				block = readCrawBlock(buf[base+pixelBlockSize : base+pixelBlockSize+pixelBlockSize]) // idem
				green := block.Decompress()

				for ir := range red {
					red[ir] = pixel(process(uint32(red[ir]), uint32(rw.blackLevel[0]), whiteBalanceRGGB[0]))
				}

				for ir := range green {
					green[ir] = pixel(process(uint32(green[ir]), uint32(rw.blackLevel[1]), whiteBalanceRGGB[1]))
				}
				for i := 0; i < pixelBlockSize; i++ {
					img.Pix[base+(i*2)].R = uint16(red[i])
					img.Pix[base+(i*2)+1].G = uint16(green[i])
				}
			} else {
				//fmt.Printf("Green block on line: %v\t column: %v\n", y, x)
				base := y*img.Stride + x

				block := readCrawBlock(buf[base : base+pixelBlockSize]) //16 red pixels, inverleaved with following 16 green
				green := block.Decompress()

				//fmt.Printf("Green block on line: %v\t column: %v\n", y, x+pixelBlockSize)
				block = readCrawBlock(buf[base+pixelBlockSize : base+pixelBlockSize+pixelBlockSize]) // idem
				blue := block.Decompress()

				for ir := range green {
					green[ir] = pixel(process(uint32(green[ir]), uint32(rw.blackLevel[0]), whiteBalanceRGGB[0]))
				}

				for ir := range blue {
					blue[ir] = pixel(process(uint32(blue[ir]), uint32(rw.blackLevel[1]), whiteBalanceRGGB[1]))
				}
				for i := 0; i < pixelBlockSize; i++ {
					img.Pix[base+(i*2)].G = uint16(green[i])
					img.Pix[base+(i*2)+1].B = uint16(blue[i])
				}
			}
		}
	}
	for y := 0; y < img.Rect.Max.Y; y++ {
		for x := 0; x < img.Rect.Max.X; x++ {
			img.Pix[y*img.Stride+x].G = img.Pix[y*img.Stride+x+1].G
			img.Pix[y*img.Stride+x].B = img.Pix[(y+1)*img.Stride+x+1].B
			x++
			img.Pix[y*img.Stride+x].R = img.Pix[y*img.Stride+x-1].R
			img.Pix[y*img.Stride+x].B = img.Pix[(y+1)*img.Stride+x].B
		}
		y++

		for x := 0; x < img.Rect.Max.X; x++ {
			img.Pix[y*img.Stride+x].R = img.Pix[(y-1)*img.Stride+x].R
			img.Pix[y*img.Stride+x].B = img.Pix[y*img.Stride+x+1].B
			x++
			img.Pix[y*img.Stride+x].R = img.Pix[(y-1)*img.Stride+x-1].R
			img.Pix[y*img.Stride+x].G = img.Pix[y*img.Stride+x-1].G
		}
	}

	return img
}

func readraw14(buf []byte, rw rawDetails) *RGB14 {
	sliceheader := *(*reflect.SliceHeader)(unsafe.Pointer(&buf))
	sliceheader.Len /= 2
	sliceheader.Cap /= 2
	data := *(*[]uint16)(unsafe.Pointer(&sliceheader))

	img := NewRGB14(image.Rect(0, 0, int(rw.width), int(rw.height)))

	var cur uint32
	var gamma [4]uint32
	gamma[0] = uint32(rw.gammaCurve[0])
	gamma[1] = uint32(rw.gammaCurve[1])
	gamma[2] = uint32(rw.gammaCurve[2])
	gamma[3] = uint32(rw.gammaCurve[3])
	gammacorrect(gamma)

	var whiteBalanceRGGB [4]float64
	var maxBalance int16
	if rw.WhiteBalance[0] > rw.WhiteBalance[1] {
		maxBalance = rw.WhiteBalance[0]
	} else {
		maxBalance = rw.WhiteBalance[1]
	}
	if rw.WhiteBalance[2] > maxBalance {
		maxBalance = rw.WhiteBalance[2]
	}
	if rw.WhiteBalance[3] > maxBalance {
		maxBalance = rw.WhiteBalance[3]
	}

	whiteBalanceRGGB[0] = float64(rw.WhiteBalance[0]) / float64(maxBalance)
	whiteBalanceRGGB[1] = float64(rw.WhiteBalance[1]) / float64(maxBalance)
	whiteBalanceRGGB[2] = float64(rw.WhiteBalance[2]) / float64(maxBalance)
	whiteBalanceRGGB[3] = float64(rw.WhiteBalance[3]) / float64(maxBalance)

	log.Println(whiteBalanceRGGB)

	for y := 0; y < img.Rect.Max.Y; y++ {
		for x := 0; x < img.Rect.Max.X; x++ {
			cur = uint32(data[y*img.Stride+x])
			cur = process(cur, uint32(rw.blackLevel[0]), whiteBalanceRGGB[0])
			img.Pix[y*img.Stride+x].R = uint16(cur)
			x++

			cur = uint32(data[y*img.Stride+x])
			cur = process(cur, uint32(rw.blackLevel[1]), whiteBalanceRGGB[1])
			img.Pix[y*img.Stride+x].G = uint16(cur)
		}
		y++

		for x := 0; x < img.Rect.Max.X; x++ {
			cur = uint32(data[y*img.Stride+x])
			cur = process(cur, uint32(rw.blackLevel[2]), whiteBalanceRGGB[2])
			img.Pix[y*img.Stride+x].G = uint16(cur)
			x++

			cur = uint32(data[y*img.Stride+x])
			cur = process(cur, uint32(rw.blackLevel[3]), whiteBalanceRGGB[3])
			img.Pix[y*img.Stride+x].B = uint16(cur)
		}
	}

	for y := 0; y < img.Rect.Max.Y; y++ {
		for x := 0; x < img.Rect.Max.X; x++ {
			img.Pix[y*img.Stride+x].G = img.Pix[y*img.Stride+x+1].G
			img.Pix[y*img.Stride+x].B = img.Pix[(y+1)*img.Stride+x+1].B
			x++
			img.Pix[y*img.Stride+x].R = img.Pix[y*img.Stride+x-1].R
			img.Pix[y*img.Stride+x].B = img.Pix[(y+1)*img.Stride+x].B
		}
		y++

		for x := 0; x < img.Rect.Max.X; x++ {
			img.Pix[y*img.Stride+x].R = img.Pix[(y-1)*img.Stride+x].R
			img.Pix[y*img.Stride+x].B = img.Pix[y*img.Stride+x+1].B
			x++
			img.Pix[y*img.Stride+x].R = img.Pix[(y-1)*img.Stride+x-1].R
			img.Pix[y*img.Stride+x].G = img.Pix[y*img.Stride+x-1].G
		}
	}

	return img
}

// NewRGBA returns a new RGBA image with the given bounds.
func NewRGB14(r image.Rectangle) *RGB14 {
	w, h := r.Dx(), r.Dy()
	buf := make([]pixel16, w*h)
	return &RGB14{buf, w, r}
}

// RGBA64 is an in-memory image whose At method returns pixel16 values.
type RGB14 struct {
	Pix []pixel16
	// Stride is the Pix stride between vertically adjacent pixels.
	Stride int
	// Rect is the image's bounds.
	Rect image.Rectangle
}

func (r *RGB14) at(x, y int) pixel16 {
	return r.Pix[(y*r.Stride)+x]
}

func (r *RGB14) At(x, y int) color.Color {
	return r.at(x, y)
}

func (r *RGB14) Bounds() image.Rectangle {
	return r.Rect.Bounds()
}

func (r *RGB14) ColorModel() color.Model {
	return color.RGBA64Model
}

func (c pixel16) RGBA() (r, g, b, a uint32) {
	return uint32(c.R << 2), uint32(c.G << 2), uint32(c.B << 2), 0xffff
	//return uint32(c.R), uint32(c.G), uint32(c.B), 0xffff
}

func (r *RGB14) set(x, y int, pixel pixel16) {
	r.Pix[y*r.Stride+x] = pixel
}

type pixel16 struct {
	R uint16
	G uint16
	B uint16
	_ uint16
}
