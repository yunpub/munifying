package unifying

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/sigurn/crc16"
	"os"
	"strings"
)

type FirmwareTargetType byte

const (
	FIRMWARE_TARGET_TYPE_UNKNOWN FirmwareTargetType = 0x00
	FIRMWARE_TARGET_TYPE_NORDIC  FirmwareTargetType = 0x01
	FIRMWARE_TARGET_TYPE_TI      FirmwareTargetType = 0x02
)

type Firmware struct {
	RawData      []byte
	Size         uint16
	StartOffset  uint16
	LastOffset   uint16
	HasBL        bool
	CRC          uint16
	TailPos      uint16
	Signature    [256]byte
	HasSignature bool
	TargetType   FirmwareTargetType
}

func (f *Firmware) pushRawHexLine(hexline []byte) (err error) {
	if hexline == nil || len(hexline) < 4 {
		return errors.New("invalid")
	}

	length := hexline[0]
	addr := int(hexline[1])<<8 | int(hexline[2])
	target := hexline[3] // 0x00 - firmware data, 0xfd - signature data
	resultsize := addr + int(length)
	data := hexline[4 : 4+length]

	switch target {
	case 0x00:
		// firmware data
		if f.RawData == nil {
			f.StartOffset = uint16(addr)
			f.RawData = make([]byte, 0)
		}

		//append 0xFF till new size requirement is met
		if len(f.RawData) < resultsize {
			tail := make([]byte, resultsize-len(f.RawData))
			for i, _ := range tail {
				tail[i] = 0xFF
			}
			f.RawData = append(f.RawData, tail...)
		}

		//copy in new data
		//fmt.Printf("Appended data at %#04x\n",addr)
		copy(f.RawData[addr:resultsize], data)
		if uint16(addr) < f.StartOffset {
			f.StartOffset = uint16(addr)
		}
		if uint16(resultsize) > f.Size+f.StartOffset {
			f.Size = uint16(resultsize)-f.StartOffset
			f.LastOffset = f.StartOffset + f.Size - 1
		}
	case 0xfd:
		// signature data
		if !f.HasSignature {
			fmt.Println("signature data added")
		}
		f.HasSignature = true
		if resultsize > 0x100 {
			return errors.New("invalid signature data, out of bounds")
		}
		copy(f.Signature[addr:resultsize], data)
	default:
		//invalid
		return errors.New("invalid")
	}

	return
}

func (f *Firmware) AddSignature(sig []byte) (err error) {
	fmt.Printf("signature length length: %#x (%d) bytes\n", len(sig), len(sig))

	if len(sig) != 256 {
		f.HasSignature = false
		return errors.New("wrong size of firmware signature")
	}
	copy(f.Signature[:], sig)
	f.HasSignature = true
	return
}

func (f *Firmware) BaseImage() (img []byte, err error) {
	img = make([]byte, f.Size)
	copy(img, f.RawData[f.StartOffset:f.StartOffset+f.Size])
	return
}

/*
Firmware images are either meant for <=BOT03.01 (unsigned) or BOT03.02 (signed)
Images for BOT03.01 have a start address of 0x0400 and end address of 0x6bff, while images for BOT03.02 start at 0x0400
and end at 0x63ff.

It wouldn't be a good idea to convert an image for BOT03.01 to BOT03.02, because no valid signature could be provided
after modding the image (bootloader only allows flashing with signature).

Downgrading an image for BOT03.02 to BOT03.01 is possible, though, because there is no signature check.
The following steps have to be done, in order to downgrade an image:
1) the image has to be resized from 0x6000 bytes to 0x6800 bytes (change last address from 0x63ff to 0x6bff), this
involves:
    - appending 0xFF bytes
    - moving the end marker '\xfe\xac\xad\xde' to the new image end location
    - recalculate the CRC for the new image (uint16 in directly before end marker)

2) Patching the image

If the resized image would be flashed onto a device with bootloader 03.01, it would run exactly once - for successive
boots, the dongle would be stuck in bootloader mode. This is because all firmwares assume that device data has to be
stored in one of the two flash pages, directly following the firmware end-address.

A firmware for BOT03.01 (ending at 0x6bff) assumes device data at 0x6c00/0x7000.
A firmware for BOT03.02 (ending at 0x63ff) assumes device data at 0x6400/0x6800.

The Texas Instruments Unifying receivers use a 8051 compatible MCU. This MCU runs a "Harvard Architecture", which means
code and data storage are physically separated. The TI CC2544 has a memory mapping, where the 32KB flash storage are
re-mapped into "external data" (XDATA), starting at address 0x8000.
That means from MCU perspective (runtime) firmware code has the same mapping as in a firmware file (code at offset 0x0400
in firmware, maps to code at offset 0x0400 in CODE Memory at runtime). Once a firmware is flashed, the whole mix of code
and data contained in the firmware file, could be accessed by the MCU as DATA, too (remember: code and data are two
dedicated address spaces, both starting from 0x0000 on this architecture). In contrast to the CODE memory - where the
flash content is mapped to 0x0000, the flash content for DATA memory is mapped to 0x8000.
This means if the firmware file address 0x0400 is accessed as code, it maps to 0x0400. If it is accessed as data it maps
to 0x8400 (=0x0400 + 0x8000).

So why is all of this of importance?
Because code accessing data at 0x6400/0x6800 has to be patched to access 0x6c00/0x7000, instead (to allow re-targeting
from BOT03.02 to BOT03.01). As this memory regions are considered to contain device data, they are accessed as data.
This again means: The code accessing this regions has to add an offset of 0x8000.
Thus for firmwares build for BOT03.02, device data access goes to 0xe400/0xe800, which has to be remapped to access
addresses 0xec00/0xf000, in order to get compatible to BOT03.01.

Data access to those offsets are mostly done utilizing the DPTR register, thus the following kind of instructions could
be easily patched:

	BOT03.02 version:
        90e400         mov dptr, #0xe400
        e0             movx a, @dptr

	Downgraded version for BOT03.01
        90e400         mov dptr, #0xe400
        e0             movx a, @dptr

Beside several `mov DPTR,<XDATA address>` instructions, more complicated code needs to be adjusted in addition (mostly
loop counters and code using only the MSB part of the device data address). Because of this, it is not easy to implement
a generic patching system, working in search-and-replace-fashion. So the following method is only an attempt to automatically
patch a firmware for downgrade. It does not give any guarantees for a working results.


 */
func (f *Firmware) BaseImageDowngradeFromBL0302ToBL0301() (patched_baseimage []byte, err error) {
	if f.TargetType != FIRMWARE_TARGET_TYPE_TI {
		return nil, errors.New("error: downgrade only supported for CC2544 firmware")
	}

	if f.Size != 0x6000 {
		err = errors.New("can't downgrade an image which hasn't a size of 0x6000")
		return
	}

	//grab a copy of the base image
	patched_baseimage = make([]byte, f.Size+0x800)
	copy(patched_baseimage, f.RawData[f.StartOffset:f.StartOffset+f.Size])

	fmt.Println("... resizing firmware")
	//overwrite image CRC and end marker with 0xFF
	for i := 0; i < 6; i++ {
		patched_baseimage[0x6000-6+i] = 0xFF
	}

	// fill appended data with 0xFF
	for i := 0x6000; i < len(patched_baseimage); i++ {
		patched_baseimage[i] = 0xFF
	}

	/*
	CAUTION: The following patch-set was only tested for working downgrades of RQR39.04 (G-Series G603 receiver)
	and RQR24.07 (latest Unifying firmware for TI receiver, downgrade basically ends up being 24.06).
	It is likely that wrong results are produced on other firmwares.

	It very likely works for RQR41.00 (SPOTLIGHT receiver firmware) and RQR45.00 (R500 receiver firmware).
	 */

	// Apply patches
	//1		90e400		-->	90ec00
	//2		7a047be4	-->	7a047bec
	//3		90e800		-->	90f000
	//4		7a047be8	-->	7a047bf0
	//5		0874e4		-->	0874ec
	//6		750fe8		-->	750ff0
	//7		791a		-->	791c
	//8		7f1a797f	-->	7f1c797f
	//9		7f19		-->	7f1b
	//10	7919		-->	791b
	//11	f20874e8	-->	f20874f0
	//12	0fe422		-->	0fec22
	//13	007b64		-->	007b6c
	//14	057919		-->	05791b

	fmt.Println("... patching firmware")
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x90, 0xe4, 0x00}, []byte{0x90, 0xec, 0x00}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x7a, 0x04, 0x7b, 0xe4}, []byte{0x7a, 0x04, 0x7b, 0xec}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x90, 0xe8, 0x00}, []byte{0x90, 0xf0, 0x00}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x7a, 0x04, 0x7b, 0xe8}, []byte{0x7a, 0x04, 0x7b, 0xf0}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x08, 0x74, 0xe4}, []byte{0x08, 0x74, 0xec}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x75, 0x0f, 0xe8}, []byte{0x75, 0x0f, 0xf0}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x79, 0x1a}, []byte{0x79, 0x1c}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x7f, 0x1a, 0x79, 0x7f}, []byte{0x7f, 0x1c, 0x79, 0x7f}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x7f, 0x19}, []byte{0x7f, 0x1b}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x79, 0x19}, []byte{0x79, 0x1b}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0xf2, 0x08, 0x74, 0xe8}, []byte{0xf2, 0x08, 0x74, 0xf0}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x0f, 0xe4, 0x22}, []byte{0x0f, 0xec, 0x22}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x00, 0x7b, 0x64}, []byte{0x00, 0x7b, 0x6c}, -1)
	patched_baseimage = bytes.Replace(patched_baseimage, []byte{0x05, 0x79, 0x19}, []byte{0x05, 0x79, 0x1b}, -1)

	//put in the new end marker
	copy(patched_baseimage[len(patched_baseimage)-4:], []byte{0xfe, 0xc0, 0xad, 0xde})

	//recalculate CRC
	fmt.Println("... recalculating firmware CRC")
	calculated_crc := crc16.Checksum(patched_baseimage[:len(patched_baseimage)-6], crc16.MakeTable(crc16.CRC16_CCITT_FALSE)) //only regard data up to CRC offset
	patched_baseimage[len(patched_baseimage)-6] = byte(calculated_crc & 0x00ff)
	patched_baseimage[len(patched_baseimage)-5] = byte(calculated_crc >> 8)

	return

}

func (f *Firmware) String() string {
	res := ""
	res += fmt.Sprintf("Size %#04x start: %#04x end %#04x CRC %#04x\n", f.Size, f.StartOffset, f.LastOffset, f.CRC)
	return res
}

func (f *Firmware) ParseFirmwareTI() (err error) {
	// if a bootloader is present the following data is present
	// - 0x03f8 uint16, USB VID (LE)
	// - 0x03fa uint16, USB PID (LE)
	// - 0x03fc byte, BL major
	// - 0x03fd byte, BL minor
	// - 0x03fe uint16, BL Build number
	assumed_bootloader := f.RawData[:0x0400]

	// check USB VID in order to determine if a BL is prepended to the firmware blob (Logitech VID is 0x046d)
	if (assumed_bootloader[0x3f8] == 0x6d && assumed_bootloader[0x3f9] == 0x04) {
		f.HasBL = true
		f.StartOffset = 0x400
		fmt.Println("...firmware blob has a bootloader prepended")
	} else {
		f.HasBL = false
		f.StartOffset = 0x0000
		fmt.Println("...firmware blob has no bootloader prepended")
	}

	// ToDo: The firmware type could be determined from bootloader PID
	if pos := strings.Index(string(f.RawData[f.StartOffset:]), "\xfe\xc0\xad\xde"); pos < 0 {
		//can't find magic bytes
		return errors.New("seems to be no valid Logitech firmware for TI, magic bytes missing")
	} else {
		f.Size = uint16(pos) + 4
		f.LastOffset = f.Size + f.StartOffset - 1
		f.TailPos = f.StartOffset + f.Size - 6
	}

	//	fmt.Println(f.String())

	// extract CRC
	f.CRC = uint16(f.RawData[f.TailPos+1])<<8 | uint16(f.RawData[f.TailPos])

	// check CRC
	calculated_crc := crc16.Checksum(f.RawData[f.StartOffset:f.StartOffset+f.Size-6], crc16.MakeTable(crc16.CRC16_CCITT_FALSE))
	if calculated_crc != f.CRC {
		return errors.New(fmt.Sprintf("Firmware has wrong CRC (inteded %#04x, found %#04x)", calculated_crc, f.CRC))
	}
	fmt.Printf("...firmware CRC correct: %04x\n", calculated_crc)

	return nil

}

func (f *Firmware) ParseFirmwareNordic() (err error) {
	// check USB VID in order to determine if a BL is prepended to the firmware blob (Logitech VID is 0x046d)
	if len(f.RawData) > 0x7400 && f.RawData[0x7400+0xbb0] == 0x04 && f.RawData[0x7400+0xbb1] == 0x6d {
		f.HasBL = true
		fmt.Println("...firmware blob has a bootloader appended")
	} else {
		f.HasBL = false
		fmt.Println("...firmware blob has no bootloader appended")
	}

	var crc_calc uint16
	if len(f.RawData) < 0x6400 {
		goto invalid_crc
	}

	// check CRC, assuming image size 0x6400
	f.StartOffset = 0x0000
	f.LastOffset = 0x63ff
	f.Size = 0x6400
	f.CRC = uint16(f.RawData[f.Size-2])<<8 | uint16(f.RawData[f.Size-1])

	crc_calc = crc16.Checksum(f.RawData[:f.Size-2], crc16.MakeTable(crc16.CRC16_CCITT_FALSE))
	if crc_calc == f.CRC {
		fmt.Printf("...firmware CRC correct: %04x\n", crc_calc)
		return nil
	}

	if len(f.RawData) < 0x6800 {
		goto invalid_crc
	}

	// repeat CRC check, assuming image size 0x6800
	f.StartOffset = 0x0000
	f.LastOffset = 0x67ff
	f.Size = 0x6800
	f.CRC = uint16(f.RawData[f.Size-2])<<8 | uint16(f.RawData[f.Size-1])

	crc_calc = crc16.Checksum(f.RawData[:f.Size-2], crc16.MakeTable(crc16.CRC16_CCITT_FALSE))
	if crc_calc == f.CRC {
		fmt.Printf("...firmware CRC correct: %04x\n", crc_calc)
		return nil
	}

invalid_crc:
	return errors.New("No valid firmware image")
}

func ParseFirmwareBin(binblob []byte) (f *Firmware, err error) {
	fmt.Println("Parsing raw firmware blob ...")
	f = &Firmware{}
	f.RawData = binblob

	f.TargetType = FIRMWARE_TARGET_TYPE_UNKNOWN
	err = f.ParseFirmwareTI()
	if err != nil {
		fmt.Printf("No Texas Instruments firmware: %v\n", err)
		// seems to be no TI firmware, try to parse as Nordic
		errNordic := f.ParseFirmwareNordic()
		if errNordic != nil {
			fmt.Printf("No Nordic firmware: %v\n", errNordic)
			return nil, errors.New("unsupported firmware format - neither nordic, nor TI")
		}
		fmt.Println("...provided firmware targets Nordic based receiver")
		f.TargetType = FIRMWARE_TARGET_TYPE_NORDIC
	} else {
		f.TargetType = FIRMWARE_TARGET_TYPE_TI
		fmt.Println("...provided firmware targets Texas Instruments based receiver")
	}

	return f, nil
}

func ParseFirmwareHex(ihex_file_path string) (f *Firmware, err error) {
	fmt.Printf("Parsing firmware hex file '%s'\n", ihex_file_path)

	file, err := os.Open(ihex_file_path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	f = &Firmware{}

	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()[1:]
		hbytes, err := hex.DecodeString(line)
		if err != nil {
			fmt.Printf("Skip invalid line %d: %s\n", lineNo, line)
			continue
		}
		if len(hbytes) < 4 || (hbytes[3] != 0x00 && hbytes[3] != 0xfd) {
			// skip lines which are too short or out of interest
			continue
		}
		//fmt.Printf("%4d: % 02x\n", lineNo, hbytes)
		f.pushRawHexLine(hbytes)
	}

	// trim down firmware to get rid of prepended data
	f.RawData = f.RawData[f.StartOffset:f.StartOffset+f.Size]

	//fmt.Printf("FWIRMWAR\n%02x\n", f.RawData)

	fmt.Println("Determin firmware type...")
	f.TargetType = FIRMWARE_TARGET_TYPE_UNKNOWN
	err = f.ParseFirmwareTI()
	if err != nil {
		fmt.Printf("No Texas Instruments firmware: %v\n", err)
		// seems to be no TI firmware, try to parse as Nordic
		errNordic := f.ParseFirmwareNordic()
		if errNordic != nil {
			fmt.Printf("No Nordic firmware: %v\n", errNordic)
			return nil, errors.New("unsupported firmware format - neither nordic, nor TI")
		}
		fmt.Println("Provided firmware targets Nordic based receiver")
		f.TargetType = FIRMWARE_TARGET_TYPE_NORDIC
	} else {
		f.TargetType = FIRMWARE_TARGET_TYPE_TI
		fmt.Println("Provided firmware targets Texas Instruments based receiver")
	}

	return f, nil
}
