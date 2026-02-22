# ACOM Amplifier Monitor

`acom-mon` is a Go-based service designed to monitor and control ACOM 600S and 1200S solid-state amplifiers via their RS-232 serial interface. It decodes the amplifier's proprietary telemetry protocol and integrates seamlessly with Home Assistant via MQTT Auto Discovery.

## Features

- **Real-time Telemetry**: Reads power, SWR, temperature, frequency, band, and error states.
- **Power Smoothing**: Implements a moving average for Forward Power to provide stable readings.
- **Home Assistant Integration**: Automatically creates sensors and switches in Home Assistant using MQTT Discovery.
- **Control**: Allows switching the amplifier between Standby (STB) and Operate (OPR) modes remotely.
- **Watchdog**: Ensures the "Enable Telemetry" command is re-sent if the amplifier is power-cycled or the connection is reset.

## Usage

Build the binary:
```bash
go build -o acom-mon acom-mon.go
```

Run the service:
```bash
./acom-mon [flags]
```

### Command Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-port` | Path to the serial device | `/dev/serial/by-id/...` |
| `-mqtt-host` | MQTT Broker IP address or hostname | `192.168.1.50` |
| `-mqtt-user` | MQTT Username | `hf` |
| `-mqtt-pass` | MQTT Password | *(empty)* |
| `-avg-time` | Moving average window (ms) for power readings | `300` |
| `-debug` | Enable verbose hex dump logging of serial I/O | `false` |

### Example

```bash
./acom-mon -port /dev/ttyUSB0 -mqtt-host 192.168.1.100 -mqtt-user myuser -mqtt-pass secret
```

## Home Assistant Sensors

When running, the service publishes configuration payloads to the `homeassistant/` topic. The following entities will appear in Home Assistant:

### Sensors
- **Forward Power** (`sensor.acom_amplifier_forward_power`): Measured in Watts.
- **Reflected Power** (`sensor.acom_amplifier_reflected_power`): Measured in Watts.
- **Input Power** (`sensor.acom_amplifier_input_power`): Drive power in Watts.
- **SWR** (`sensor.acom_amplifier_swr`): Standing Wave Ratio.
- **Temperature** (`sensor.acom_amplifier_temperature`): PA temperature in °C.
- **Frequency** (`sensor.acom_amplifier_frequency`): Operating frequency (during TX) in kHz 
- **Band** (`sensor.acom_amplifier_band`): Current band (e.g., 20m, 40m).
- **Mode** (`sensor.acom_amplifier_mode`): Current state (e.g., OPR/TX, STANDBY, OFF).
- **Status** (`sensor.acom_amplifier_status`): Error messages or warnings (e.g., "NONE", "HIGH SWR").

### Switches
- **Operate** (`switch.acom_amplifier_operate`):
  - **ON**: Sets amplifier to `OPR` (Operate) mode.
  - **OFF**: Sets amplifier to `STB` (Standby) mode.

## Requirements
- ACOM 600S or 1200S Amplifier connected via RS-232 
- MQTT Broker (e.g., Mosquitto).
