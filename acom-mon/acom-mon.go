package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"go.bug.st/serial"
)

// Constants defined in the protocol documentation
const (
	StartByte     byte = 0x55 // Start byte
	MsgEnableAuto byte = 0x92 // Enable automatic telemetry
	MsgTelemetry  byte = 0x2F // Combined status message
	MsgAck        byte = 0x86 // Acknowledge message
	MsgAmpMgmt    byte = 0x81 // Commands for amplifier management

	// Sub-commands for MsgAmpMgmt (Byte 3)
	CmdModeChange byte = 0x02 // Request for amplifier mode change

	// Modes for CmdModeChange (Byte 5)
	ModeSTB   byte = 0x05 // STB Mode
	ModeOPRRX byte = 0x06 // OPR/RX Mode

	SerialPort = "/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0"
)

// MQTT Constants for Home Assistant
const (
	DiscoveryPrefix = "homeassistant"
	NodeID          = "acom_amplifier"
	StateTopic      = "acom/state"
	ControlTopic    = "acom/control/operate/set"
)

// Global state
var (
	debugMode  bool
	activePort serial.Port
	portMu     sync.Mutex
	shutdown   bool
	mqttClient mqtt.Client

	// State tracking
	currentMode string
	modeMu      sync.RWMutex
)

// PowerSample holds a single measurement and its timestamp
type PowerSample struct {
	Value     uint16
	Timestamp time.Time
}

// PowerAverager handles the time-based moving average
type PowerAverager struct {
	windowDuration time.Duration
	samples        []PowerSample
	mu             sync.Mutex
}

func NewPowerAverager(durationMs int) *PowerAverager {
	if durationMs < 1 {
		durationMs = 1
	}
	return &PowerAverager{
		windowDuration: time.Duration(durationMs) * time.Millisecond,
		samples:        make([]PowerSample, 0, 50),
	}
}

func (pa *PowerAverager) Add(value uint16) uint16 {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	now := time.Now()
	pa.samples = append(pa.samples, PowerSample{Value: value, Timestamp: now})

	cutoff := now.Add(-pa.windowDuration)
	validStartIndex := 0
	for i, s := range pa.samples {
		if s.Timestamp.After(cutoff) {
			validStartIndex = i
			break
		}
	}
	if validStartIndex > 0 {
		pa.samples = pa.samples[validStartIndex:]
	}

	if len(pa.samples) == 0 {
		return 0
	}
	var sum uint32
	for _, s := range pa.samples {
		sum += uint32(s.Value)
	}
	return uint16(sum / uint32(len(pa.samples)))
}

// TelemetryPayload defines the JSON data sent to 'acom/state'
type TelemetryPayload struct {
	ForwardPower   uint16  `json:"fwd"`
	ReflectedPower uint16  `json:"ref"`
	InputPower     float64 `json:"in"`
	SWR            float64 `json:"swr"`
	Temperature    float64 `json:"temp"`
	Frequency      uint16  `json:"freq"`
	Band           string  `json:"band"`
	Mode           string  `json:"mode"`
	ErrorCode      string  `json:"err_code"`
	ErrorMsg       string  `json:"err_msg"`
}

// HADiscoveryConfig defines the payload for HA Auto Discovery
type HADiscoveryConfig struct {
	Name              string   `json:"name"`
	StateTopic        string   `json:"state_topic"`
	CommandTopic      string   `json:"command_topic,omitempty"`
	UniqueID          string   `json:"unique_id"`
	ValueTemplate     string   `json:"value_template,omitempty"`
	UnitOfMeasurement string   `json:"unit_of_measurement,omitempty"`
	DeviceClass       string   `json:"device_class,omitempty"`
	StateClass        string   `json:"state_class,omitempty"`
	PayloadOn         string   `json:"payload_on,omitempty"`
	PayloadOff        string   `json:"payload_off,omitempty"`
	Icon              string   `json:"icon,omitempty"`
	Device            HADevice `json:"device"`
}

type HADevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	Manufacturer string   `json:"manufacturer"`
}

func main() {
	flag.BoolVar(&debugMode, "debug", false, "Enable hex dump of serial I/O")
	portPath := flag.String("port", SerialPort, "Serial port path")
	mqttPass := flag.String("mqtt-pass", "", "MQTT Password")
	avgTimeMs := flag.Int("avg-time", 300, "Time window in milliseconds for Forward Power moving average")
	flag.Parse()

	setupSignalHandler()
	setupMQTT("192.168.1.50", "hf", *mqttPass)

	// Start the Watchdog
	go telemetryWatchdog()

	log.Printf("Starting ACOM Monitor Service on %s (Avg Window: %dms)...", *portPath, *avgTimeMs)

	for {
		if shutdown {
			break
		}
		err := runMonitor(*portPath, *avgTimeMs)
		if shutdown {
			break
		}
		if err != nil {
			log.Printf("Monitor error: %v. Restarting loop in 5s...", err)
		}
		time.Sleep(5 * time.Second)
	}
}

// telemetryWatchdog checks every 3 seconds if the Mode is "OFF".
// If so, it blindly sends the Enable Telemetry command to ensure
// data resumes immediately when the user switches the Amp back ON.
func telemetryWatchdog() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		if shutdown {
			return
		}
		<-ticker.C

		modeMu.RLock()
		mode := currentMode
		modeMu.RUnlock()

		if mode == "OFF" {
			if debugMode {
				log.Println("Watchdog: Mode is OFF, sending Enable Telemetry command...")
			}
			if err := sendEnableTelemetry(); err != nil {
				// Don't spam logs if port is closed/reconnecting
				if debugMode {
					log.Printf("Watchdog failed to send enable: %v", err)
				}
			}
		}
	}
}

func setupMQTT(brokerIP, user, password string) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:1883", brokerIP))
	opts.SetClientID("acom_monitor_service")
	opts.SetUsername(user)
	opts.SetPassword(password)
	opts.SetAutoReconnect(true)

	opts.SetOnConnectHandler(func(client mqtt.Client) {
		log.Println("MQTT Connected")
		publishAutoDiscovery(client)
		client.Subscribe(ControlTopic, 0, handleMQTTControl)
	})

	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		log.Printf("MQTT Connection Lost: %v", err)
	})

	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Printf("MQTT Initial Connection Failed: %v", token.Error())
	}
}

func handleMQTTControl(client mqtt.Client, msg mqtt.Message) {
	payload := string(msg.Payload())
	log.Printf("Received Control Command: %s", payload)

	var targetMode byte

	if payload == "ON" {
		targetMode = ModeOPRRX // 0x06
	} else if payload == "OFF" {
		targetMode = ModeSTB // 0x05
	} else {
		log.Println("Unknown command ignored.")
		return
	}

	if err := sendSetMode(targetMode); err != nil {
		log.Printf("Failed to set mode: %v", err)
	} else {
		log.Printf("Sent SetMode command: 0x%02X", targetMode)
	}
}

// sendSetMode sends the "Commands for amplifier management" packet (0x81)
func sendSetMode(modeByte byte) error {
	portMu.Lock()
	defer portMu.Unlock()

	if activePort == nil {
		return fmt.Errorf("serial port not active")
	}

	packet := []byte{
		StartByte,     // 0x55
		MsgAmpMgmt,    // 0x81
		0x08,          // Length
		CmdModeChange, // 0x02
		0x00,          // Param HW
		modeByte,      // Target Mode
		0x00,          // Buttons
	}

	chk := calculateChecksum(packet)
	packet = append(packet, chk)

	if debugMode {
		log.Printf("TX CMD: %s", hex.EncodeToString(packet))
	}
	_, err := activePort.Write(packet)
	return err
}

func sendEnableTelemetry() error {
	portMu.Lock()
	defer portMu.Unlock()

	if activePort == nil {
		return fmt.Errorf("serial port not active")
	}

	// 0x55, 0x92, 0x04, 0x15
	cmd := []byte{StartByte, MsgEnableAuto, 0x04, 0x15}

	if debugMode {
		log.Printf("TX ENABLE: %s", hex.EncodeToString(cmd))
	}
	_, err := activePort.Write(cmd)
	return err
}

func publishAutoDiscovery(client mqtt.Client) {
	device := HADevice{
		Identifiers:  []string{"acom_serial_v1"},
		Name:         "ACOM Amplifier",
		Model:        "ACOM 600S/1200S",
		Manufacturer: "ACOM",
	}

	publishSensor := func(objectID, name, unit, devClass, stateClass, template, icon string) {
		topic := fmt.Sprintf("%s/sensor/%s/%s/config", DiscoveryPrefix, NodeID, objectID)
		payload := HADiscoveryConfig{
			Name:              name,
			StateTopic:        StateTopic,
			UniqueID:          "acom_" + objectID,
			ValueTemplate:     template,
			UnitOfMeasurement: unit,
			DeviceClass:       devClass,
			StateClass:        stateClass,
			Icon:              icon,
			Device:            device,
		}
		jsonBytes, _ := json.Marshal(payload)
		client.Publish(topic, 0, true, jsonBytes)
	}

	publishSwitch := func(objectID, name, icon string) {
		topic := fmt.Sprintf("%s/switch/%s/%s/config", DiscoveryPrefix, NodeID, objectID)
		payload := HADiscoveryConfig{
			Name:          name,
			StateTopic:    StateTopic,
			CommandTopic:  ControlTopic,
			UniqueID:      "acom_" + objectID,
			ValueTemplate: "{{ 'ON' if 'OPR' in value_json.mode else 'OFF' }}",
			PayloadOn:     "ON",
			PayloadOff:    "OFF",
			Icon:          icon,
			Device:        device,
		}
		jsonBytes, _ := json.Marshal(payload)
		client.Publish(topic, 0, true, jsonBytes)
	}

	publishSensor("fwd_pwr", "Forward Power", "W", "power", "measurement", "{{ value_json.fwd }}", "mdi:transmission-tower")
	publishSensor("ref_pwr", "Reflected Power", "W", "power", "measurement", "{{ value_json.ref }}", "mdi:flash-alert")
	publishSensor("in_pwr", "Input Power", "W", "power", "measurement", "{{ value_json.in }}", "mdi:import")
	publishSensor("swr", "SWR", "", "", "measurement", "{{ value_json.swr }}", "mdi:arrow-expand-vertical")
	publishSensor("temp", "Temperature", "°C", "temperature", "measurement", "{{ value_json.temp }}", "mdi:thermometer")
	publishSensor("freq", "Frequency", "kHz", "frequency", "measurement", "{{ value_json.freq }}", "mdi:sine-wave")
	publishSensor("band", "Band", "", "enum", "", "{{ value_json.band }}", "mdi:radio")
	publishSensor("mode", "Mode", "", "enum", "", "{{ value_json.mode }}", "mdi:radio-tower")
	publishSensor("error", "Status", "", "", "", "{{ value_json.err_msg }}", "mdi:alert-circle-outline")
	publishSwitch("operate", "Operate", "mdi:power-cycle")
}

func setupSignalHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		log.Println("Stopping service...")
		portMu.Lock()
		shutdown = true
		if activePort != nil {
			activePort.Close()
		}
		portMu.Unlock()
		if mqttClient != nil && mqttClient.IsConnected() {
			mqttClient.Disconnect(250)
		}
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()
}

func runMonitor(portName string, avgTimeMs int) error {
	mode := &serial.Mode{
		BaudRate: 9600,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}

	port, err := serial.Open(portName, mode)
	if err != nil {
		return err
	}

	if err := port.SetReadTimeout(1 * time.Second); err != nil {
		log.Printf("Warning: Failed to set read timeout: %v", err)
	}

	portMu.Lock()
	activePort = port
	portMu.Unlock()

	defer func() {
		portMu.Lock()
		activePort = nil
		portMu.Unlock()
		port.Close()
	}()

	port.ResetInputBuffer()
	port.ResetOutputBuffer()

	fwdAverager := NewPowerAverager(avgTimeMs)

	// Send initial enable
	if err := sendEnableTelemetry(); err != nil {
		return fmt.Errorf("failed to enable telemetry: %v", err)
	}
	log.Println("Telemetry Enable Command Sent.")

	rxBuf := make([]byte, 0, 1024)
	tmpBuf := make([]byte, 128)
	lastDataTime := time.Now()
	lastRetryTime := time.Now()

	for {
		n, err := port.Read(tmpBuf)
		if err != nil {
			return err
		}

		if n > 0 {
			lastDataTime = time.Now()

			rxBuf = append(rxBuf, tmpBuf[:n]...)
			rxBuf = processBuffer(port, rxBuf, fwdAverager)
		} else {
			// Timeout (n == 0)
			if time.Since(lastDataTime) > 30*time.Second {
				return fmt.Errorf("no data received for 30s, restarting monitor")
			}
			if time.Since(lastDataTime) > 5*time.Second && time.Since(lastRetryTime) > 5*time.Second {
				log.Printf("No data for 5s, re-sending Enable Telemetry...")
				if err := sendEnableTelemetry(); err != nil {
					log.Printf("Failed to re-send enable telemetry: %v", err)
				}
				lastRetryTime = time.Now()
			}
		}
	}
}

func processBuffer(port serial.Port, buf []byte, fwdAvg *PowerAverager) []byte {
	for len(buf) >= 3 {
		if buf[0] != StartByte {
			buf = buf[1:]
			continue
		}
		pktLen := int(buf[2])
		if len(buf) < pktLen {
			return buf
		}
		packet := buf[:pktLen]
		if verifyChecksum(packet) {
			handlePacket(port, packet, fwdAvg)
			buf = buf[pktLen:]
		} else {
			buf = buf[1:]
		}
	}
	return buf
}

func verifyChecksum(packet []byte) bool {
	var sum byte
	for _, b := range packet {
		sum += b
	}
	return sum == 0
}

func calculateChecksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return byte(0 - int8(sum))
}

func handlePacket(port serial.Port, packet []byte, fwdAvg *PowerAverager) {
	if debugMode {
		log.Printf("RX PKT: %s", hex.EncodeToString(packet))
	}
	addr := packet[1]
	if !shutdown {
		_ = sendAck(port, addr)
	}
	if addr == MsgTelemetry {
		parseTelemetry(packet, fwdAvg)
	}
}

func sendAck(port serial.Port, msgReceivedAddr byte) error {
	packet := []byte{StartByte, MsgAck, 0x05, msgReceivedAddr}
	chk := calculateChecksum(packet)
	packet = append(packet, chk)
	return sendPacket(port, packet)
}

func sendPacket(port serial.Port, packet []byte) error {
	if debugMode {
		log.Printf("TX: %s", hex.EncodeToString(packet))
	}
	_, err := port.Write(packet)
	return err
}

func parseTelemetry(data []byte, fwdAvg *PowerAverager) {
	if len(data) != 72 {
		return
	}

	rawFwdPwr := binary.LittleEndian.Uint16(data[22:24])
	refPwr := binary.LittleEndian.Uint16(data[24:26])
	inPwrRaw := binary.LittleEndian.Uint16(data[20:22])
	inPwr := float64(inPwrRaw) / 10.0
	swrRaw := binary.LittleEndian.Uint16(data[26:28])
	swr := float64(swrRaw) / 100.0

	tempK := binary.LittleEndian.Uint16(data[16:18])
	tempC := 0.0
	if tempK > 0 {
		tempC = float64(tempK) - 273.15
	}

	freqKHz := binary.LittleEndian.Uint16(data[48:50])
	bandByte := data[69] & 0x0F
	bandStr := decodeBand(bandByte)
	modeByte := data[3]
	modeStr := decodeMode(modeByte)
	errByte := data[66]
	errMsg := decodeError(errByte)

	// Update global state safely
	modeMu.Lock()
	currentMode = modeStr
	modeMu.Unlock()

	avgFwdPwr := fwdAvg.Add(rawFwdPwr)

	payload := TelemetryPayload{
		ForwardPower:   avgFwdPwr,
		ReflectedPower: refPwr,
		InputPower:     inPwr,
		SWR:            swr,
		Temperature:    tempC,
		Frequency:      freqKHz,
		Band:           bandStr,
		Mode:           modeStr,
		ErrorCode:      fmt.Sprintf("0x%02X", errByte),
		ErrorMsg:       errMsg,
	}

	if mqttClient != nil && mqttClient.IsConnected() {
		jsonBytes, _ := json.Marshal(payload)
		mqttClient.Publish(StateTopic, 0, false, jsonBytes)
	}
}

func decodeBand(b byte) string {
	switch b {
	case 1:
		return "160m"
	case 2:
		return "80m"
	case 3:
		return "40m"
	case 4:
		return "30m"
	case 5:
		return "20m"
	case 6:
		return "17m"
	case 7:
		return "15m"
	case 8:
		return "12m"
	case 9:
		return "10m"
	case 10:
		return "6m"
	default:
		return "UNK"
	}
}

func decodeMode(b byte) string {
	switch b & 0xF0 {
	case 0x10:
		return "RESET"
	case 0x20:
		return "INIT"
	case 0x30:
		return "DEBUG"
	case 0x40:
		return "SERVICE"
	case 0x50:
		return "STANDBY"
	case 0x60:
		return "OPR/RX"
	case 0x70:
		return "OPR/TX"
	case 0x80:
		return "ATAC"
	case 0x90:
		return "MENU"
	case 0xA0:
		return "OFF"
	default:
		return "UNKNOWN"
	}
}

func decodeError(b byte) string {
	if b == 0xFF {
		return "NONE"
	}
	if b == 0x00 {
		return "HOT SWITCHING ATTEMPT"
	}

	switch b {
	case 0x01:
		return "OUTPUT RELAY CLOSED (SHOULD BE OPEN)"
	case 0x02:
		return "OUTPUT RELAY OPEN (SHOULD BE CLOSED)"
	case 0x03:
		return "DRIVE POWER WRONG TIME"
	case 0x04:
		return "REFLECTED POWER WARNING"
	case 0x05:
		return "EXCESSIVE REFLECTED POWER"
	case 0x06:
		return "DRIVE POWER TOO HIGH"
	case 0x07:
		return "EXCESSIVE DRIVE POWER"
	case 0x08:
		return "HOT SWITCHING ATTEMPT (2)"
	case 0x09:
		return "DRIVE FREQUENCY OUT OF RANGE"
	case 0x0A:
		return "FREQUENCY VIOLATION"
	case 0x0B:
		return "OUTPUT DISBALANCE"
	case 0x0C:
		return "DETECTED RF POWER WRONG TIME"
	case 0x0D:
		return "PA LOAD SWR TOO HIGH"
	case 0x0E:
		return "STOP TRANSMISSION FIRST"
	case 0x0F:
		return "REMOVE DRIVE POWER IMMEDIATELY"
	case 0x10:
		return "5V TOO LOW"
	case 0x11:
		return "5V TOO HIGH"
	case 0x12:
		return "26V TOO LOW"
	case 0x13:
		return "26V TOO HIGH"
	case 0x14:
		return "ERROR 0x14"
	case 0x15:
		return "PAM1 FAN SPEED TOO LOW"
	case 0x16:
		return "PAM2 FAN SPEED TOO LOW"
	case 0x17:
		return "LPF FAN SPEED TOO LOW"
	case 0x18:
		return "PAM1 DISSIPATION TOO HIGH"
	case 0x19:
		return "PAM2 DISSIPATION TOO HIGH"
	case 0x1A:
		return "PAM1 DISSIPATION WARNING"
	case 0x1B:
		return "PAM2 DISSIPATION WARNING"
	case 0x1C:
		return "PAM1 TEMP TOO HIGH"
	case 0x1D:
		return "PAM2 TEMP TOO HIGH"
	case 0x1E:
		return "PAM1 EXCESSIVE TEMP"
	case 0x1F:
		return "PAM2 EXCESSIVE TEMP"
	case 0x20:
		return "PAM1 HV TOO LOW"
	case 0x21:
		return "PAM1 HV TOO HIGH"
	case 0x22:
		return "PAM1 CURRENT NON-ZERO"
	case 0x23:
		return "PAM1 IDLE CURRENT TOO LOW"
	case 0x24:
		return "PAM1 CURRENT WARNING"
	case 0x25:
		return "PAM1 EXCESSIVE CURRENT"
	case 0x26:
		return "BIAS_1A VOLTAGE ERROR"
	case 0x27:
		return "BIAS_1B VOLTAGE ERROR"
	case 0x28:
		return "BIAS_1C VOLTAGE ERROR"
	case 0x29:
		return "BIAS_1D VOLTAGE ERROR"
	case 0x2A:
		return "BIAS_1A SHOULD BE ZERO"
	case 0x2B:
		return "BIAS_1B SHOULD BE ZERO"
	case 0x2C:
		return "BIAS_1C SHOULD BE ZERO"
	case 0x2D:
		return "BIAS_1D SHOULD BE ZERO"
	case 0x2E:
		return "PAM1 GAIN TOO LOW"
	case 0x2F:
		return "PAM1 GAIN TOO HIGH"
	case 0x30:
		return "PAM1 HV SHOULD BE ZERO"
	case 0x31:
		return "PAM1 CURRENT SHOULD BE ZERO"
	case 0x32:
		return "PAM1 EXCESSIVE TEMP (3)"
	case 0x33:
		return "PAM1 TEMP TOO HIGH (3)"
	case 0x34:
		return "BIAS_1A SHOULD BE ZERO (3)"
	case 0x35:
		return "BIAS_1B SHOULD BE ZERO (3)"
	case 0x36:
		return "BIAS_1C SHOULD BE ZERO (3)"
	case 0x37:
		return "BIAS_1D SHOULD BE ZERO (3)"
	case 0x38:
		return "PSU1 EXCESSIVE TEMP"
	case 0x39:
		return "PAM1 EXCESSIVE CURRENT (CHECK SWR)"
	case 0x40:
		return "PAM2 HV TOO LOW"
	case 0x41:
		return "PAM2 HV TOO HIGH"
	case 0x42:
		return "PAM2 CURRENT NON-ZERO"
	case 0x43:
		return "PAM2 IDLE CURRENT TOO LOW"
	case 0x44:
		return "PAM2 CURRENT WARNING"
	case 0x45:
		return "PAM2 EXCESSIVE CURRENT"
	case 0x46:
		return "BIAS_2A VOLTAGE ERROR"
	case 0x47:
		return "BIAS_2B VOLTAGE ERROR"
	case 0x48:
		return "BIAS_2C VOLTAGE ERROR"
	case 0x49:
		return "BIAS_2D VOLTAGE ERROR"
	case 0x4A:
		return "BIAS_2A SHOULD BE ZERO"
	case 0x4B:
		return "BIAS_2B SHOULD BE ZERO"
	case 0x4C:
		return "BIAS_2C SHOULD BE ZERO"
	case 0x4D:
		return "BIAS_2D SHOULD BE ZERO"
	case 0x4E:
		return "PAM2 GAIN TOO LOW"
	case 0x4F:
		return "PAM2 GAIN TOO HIGH"
	case 0x60:
		return "PSU1 CONTROL MALFUNCTION"
	case 0x61:
		return "PSU2 CONTROL MALFUNCTION"
	case 0x62:
		return "PSU1 EXCESSIVE TEMP"
	case 0x63:
		return "PSU2 EXCESSIVE TEMP"
	case 0x64:
		return "DISPLAY COMM ERROR"
	case 0x65:
		return "ATU MODEM TEMP"
	case 0x66:
		return "ATU POWER SWITCH ALARM"
	case 0x67:
		return "ATU POWER SWITCH ALARM (ON)"
	case 0x68:
		return "ETHERNET NOT RESPONDING"
	case 0x69:
		return "AUDIO MEMORY ERROR"
	case 0x6C:
		return "LOSS OF AUDIO DATA"
	case 0x6D:
		return "LOSS OF ETHERNET DATA"
	case 0x6E:
		return "LOSS OF EEPROM DATA (WARN)"
	case 0x6F:
		return "LOSS OF EEPROM DATA (SOFT)"
	case 0x70:
		return "CAT ERROR"
	case 0x80:
		return "ATU NOT RESPONDING / BIAS 1A ERR"
	case 0x81:
		return "ATU-AMP COMM ERROR"
	case 0x82:
		return "AMP-ATU COMM ERROR"
	case 0x83:
		return "ASEL NOT RESPONDING"
	case 0x84:
		return "ASEL-AMP COMM ERROR"
	case 0x85:
		return "AMP-ASEL COMM ERROR"
	case 0x86:
		return "NO TUNING SETTINGS"
	case 0x87:
		return "NO ANTENNA SETTINGS"
	case 0x88:
		return "ATU CANNOT RETUNE (RF PRESENT)"
	case 0x89:
		return "ANTENNA CANNOT CHANGE (RF PRESENT)"
	case 0x8A:
		return "ATU TUNING UNSUCCESSFUL"
	case 0x8B:
		return "ATU MEMORY FAIL"
	case 0xA0:
		return "ATU DC VOLT TOO HIGH"
	case 0xA1:
		return "ATU DC VOLT TOO LOW"
	case 0xA2:
		return "ATU 5V TOO LOW"
	case 0xA3:
		return "ATU 5V TOO HIGH"
	case 0xA4:
		return "ANTENNA VOLT TOO HIGH (PWR)"
	case 0xA5:
		return "ANTENNA VOLT TOO HIGH (dmg)"
	case 0xA6:
		return "ANTENNA CURRENT TOO HIGH (PWR)"
	case 0xA7:
		return "ANTENNA CURRENT TOO HIGH (dmg)"
	case 0xA8:
		return "ANT REFL PWR TOO HIGH (SOFT)"
	case 0xA9:
		return "ANT REFL PWR TOO HIGH (HARD)"
	case 0xAA:
		return "ATU INPUT PWR TOO HIGH"
	case 0xAB:
		return "ATU INPUT PWR TOO HIGH (dmg)"
	case 0xAC:
		return "ANTENNA SWR TOO HIGH"
	case 0xAD:
		return "ANTENNA SWR TOO HIGH (dmg)"
	case 0xAE:
		return "ATU TEMP TOO HIGH"
	case 0xAF:
		return "ATU TEMP TOO LOW"
	default:
		return fmt.Sprintf("UNKNOWN ERROR (0x%02X)", b)
	}
}
