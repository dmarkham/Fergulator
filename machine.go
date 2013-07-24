package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"
)

var (
	cpuClockSpeed = 1789773
	running       = true
	audioEnabled  = true

	cpu            Cpu
	ppu            Ppu
	apu            Apu
	rom            Mapper
	video          Video
	audio          Audio
	pads           [2]*Controller
	playEvents     chan interface{}
	totalCpuCycles int

	gamename       string
	saveStateFile  string
	batteryRamFile string

	cpuprofile = flag.String("cprof", "", "write cpu profile to file")
)

const (
	SaveState = iota
	LoadState
	LearnState
	PlayState
)

func setResetVector() {
	high, _ := Ram.Read(0xFFFD)
	low, _ := Ram.Read(0xFFFC)

	ProgramCounter = (uint16(high) << 8) + uint16(low)
}

func LoadGameState() {
	fmt.Println("Loading state")

	state, err := ioutil.ReadFile(saveStateFile)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	for i, v := range state[:0x2000] {
		Ram[i] = Word(v)
	}

	pchigh := uint16(state[0x2000])
	pclow := uint16(state[0x2001])

	ProgramCounter = (pchigh << 8) | pclow

	cpu.A = Word(state[0x2002])
	cpu.X = Word(state[0x2003])
	cpu.Y = Word(state[0x2004])
	cpu.P = Word(state[0x2005])
	cpu.StackPointer = Word(state[0x2006])

	// Sprite RAM
	for i, v := range state[0x2007:0x2107] {
		ppu.SpriteRam[i] = Word(v)
	}

	// Pattern VRAM
	for i, v := range state[0x2107:0x4107] {
		ppu.Vram[i] = Word(v)
	}

	// Nametable VRAM
	for i, v := range state[0x4107:0x4507] {
		ppu.Nametables.LogicalTables[0][i] = Word(v)
	}
	for i, v := range state[0x4507:0x4907] {
		ppu.Nametables.LogicalTables[1][i] = Word(v)
	}
	for i, v := range state[0x4907:0x4D07] {
		ppu.Nametables.LogicalTables[2][i] = Word(v)
	}
	for i, v := range state[0x4D07:0x5107] {
		ppu.Nametables.LogicalTables[3][i] = Word(v)
	}

	// Palette RAM
	for i, v := range state[0x5107:0x5126] {
		ppu.PaletteRam[i] = Word(v)
	}
}

func SaveMemState(tick int) {
	fmt.Println("Saving state")
	saveSize := 0x800

	s := fmt.Sprintf("%s.learn", gamename)
	fo, _ := os.OpenFile(s, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	defer fo.Close()
	// RAM
	fo.WriteString(fmt.Sprintf("%d\t", tick))
	for _, v := range Ram[:saveSize] {
		fo.WriteString(fmt.Sprintf("%d\t", v))
	}
	fo.WriteString("\n")

}

func SaveGameState() {
	fmt.Println("Saving state")
	buf := new(bytes.Buffer)

	// RAM
	for _, v := range Ram[:0x2000] {
		buf.WriteByte(byte(v))
	}

	// ProgramCounter
	// High then low
	buf.WriteByte(byte(ProgramCounter>>8) & 0xFF)
	buf.WriteByte(byte(ProgramCounter & 0xFF))

	// CPU Registers
	buf.WriteByte(byte(cpu.A))
	buf.WriteByte(byte(cpu.X))
	buf.WriteByte(byte(cpu.Y))
	buf.WriteByte(byte(cpu.P))
	buf.WriteByte(byte(cpu.StackPointer))

	// Sprite RAM
	for _, v := range ppu.SpriteRam {
		buf.WriteByte(byte(v))
	}

	// Pattern VRAM
	for _, v := range ppu.Vram[:0x2000] {
		buf.WriteByte(byte(v))
	}

	// Nametable VRAM
	for _, v := range ppu.Nametables.LogicalTables[0] {
		buf.WriteByte(byte(v))
	}
	for _, v := range ppu.Nametables.LogicalTables[1] {
		buf.WriteByte(byte(v))
	}
	for _, v := range ppu.Nametables.LogicalTables[2] {
		buf.WriteByte(byte(v))
	}
	for _, v := range ppu.Nametables.LogicalTables[3] {
		buf.WriteByte(byte(v))
	}

	// Palette RAM
	for _, v := range ppu.PaletteRam {
		buf.WriteByte(byte(v))
	}

	if err := ioutil.WriteFile(saveStateFile, buf.Bytes(), 0644); err != nil {
		panic(err.Error())
	}
}

func loadBatteryRam() {
	fmt.Println("Loading battery RAM")

	batteryRam, err := ioutil.ReadFile(batteryRamFile)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	for i, v := range batteryRam[:0x2000] {
		Ram[0x6000+i] = Word(v)
	}
}

func saveBatteryFile() {
	buf := new(bytes.Buffer)

	// Battery/Work RAM
	for _, v := range Ram[0x6000:0x7FFF] {
		buf.WriteByte(byte(v))
	}

	if err := ioutil.WriteFile(batteryRamFile, buf.Bytes(), 0644); err != nil {
		panic(err.Error())
	}

	fmt.Println("Battery RAM saved to disk")
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	if len(os.Args) < 2 {
		fmt.Println("Please specify a ROM file")
		return
	}

	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}

		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	pads[0] = new(Controller)
	pads[1] = new(Controller)
	pads[0].Init(0)
	pads[1].Init(0)

	playEvents = make(chan interface{}, 1)

	Ram.Init()
	cpu.Init()
	v, ft := ppu.Init()
	al := apu.Init()

	if contents, err := ioutil.ReadFile(os.Args[len(os.Args)-1]); err == nil {

		if rom, err = LoadRom(contents); err != nil {
			fmt.Println(err.Error())
			return
		}

		// Set the game name for save states
		path := strings.Split(os.Args[1], "/")
		gamename = strings.Split(path[len(path)-1], ".")[0]
		saveStateFile = fmt.Sprintf(".%s.state", gamename)
		batteryRamFile = fmt.Sprintf(".%s.battery", gamename)

		if rom.BatteryBacked() {
			loadBatteryRam()
			defer saveBatteryFile()
		}

		setResetVector()
	} else {
		fmt.Println(err.Error())
		return
	}

	r := video.Init(v, ft, gamename)

	interrupt := make(chan int)

	a := NewAudio(al)
	defer a.Close()

	go a.Run()

	// Main runloop, in a separate goroutine so that
	// the video rendering can happen on this one
	go func(c <-chan int) {
		var lastApuTick int
		var cycles int
		var flip int
		var mems bool = false
		saveNow := time.NewTicker(time.Millisecond * 300)

		for running {
			select {
			case <-saveNow.C:
				if mems {
					SaveMemState(totalCpuCycles)
				}
			case s := <-c:
				switch s {
				case LoadState:
					LoadGameState()
				case SaveState:
					SaveGameState()
				case LearnState:
					if mems == true {
						mems = false
					} else {
						mems = true
					}
				}
			default:
				cycles = cpu.Step()
				totalCpuCycles += cycles
				for i := 0; i < 3*cycles; i++ {
					ppu.Step()
				}

				for i := 0; i < cycles; i++ {
					apu.Step()
				}

				if audioEnabled {
					if totalCpuCycles-apu.LastFrameTick >= (cpuClockSpeed / 240) {
						apu.FrameSequencerStep()
						apu.LastFrameTick = totalCpuCycles
					}

					if totalCpuCycles-lastApuTick >= ((cpuClockSpeed / 44100) + flip) {
						apu.PushSample()
						lastApuTick = totalCpuCycles

						flip = (flip + 1) & 0x1
					}
				}
			}
		}
	}(interrupt)

	go ReadInput(r, interrupt)

	// This needs to happen on the main thread for OSX
	runtime.LockOSThread()
	video.Render()

	return
}
