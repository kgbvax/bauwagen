# Antenna switch implementation
I don't want to use a StationMaster because it's too expensive and too closed for my taste.

## My requirements:

* Automatically select antenna based on band
* Allow manual selection when there is an alternative
* Allow RX antennas
* Allow easy remote control 
* Share data with Home-Assistant / MQTT because we can

## Approach
* PA reads band voltage from TRX and performs auto selection
* Pi reads frequency of active VFO from CI-V (Which is not straighforward due to shoddy design of ICOM's CI-V protocol)
  * foo
* If band change detected and new antenna is required OR
* Antenna change is manually requested then perform antenna change

## Antenna Change Procedure
To avoid hotswiching even when PTT is triggered programtatically (via CI-V, remote app etc) the proceure shall be
* Enable "Transmit Inhibit" on TRX
* Drop active antenna relay
* Activate target antenna relay
* Wait for relay to settle
* Disable "Transmit Inhibit" on TRX

# HAMLIB vs cusom


