# Antenna switching in the Bauwagen
I don't want to use a StationMaster because it's too expensive and too closed for my taste.

## My requirements:
* Automatically select antenna based on band
* Allow manual selection when there is an alternative
* Allow RX antennas and selection of antennas for 2nd reciever 
* Allow easy remote control 
* Share data with Home-Assistant / MQTT

## Approach
* PA reads band voltage from TRX and performs auto selection
* Pi reads frequency of active VFO from CI-V (Which is not straighforward due to shoddy design of ICOM's CI-V protocol)
  * foo
* If band change detected and new antenna is required OR
* Antenna change is manually requested then perform antenna change

## Antenna Change Procedure
To avoid hotswiching even when PTT is triggered programtatically (via CI-V, remote app etc) the proceure shall be
* Enable "Transmit Inhibit" on TRX. 
* (Optional: interrupt SEND line to PA, but this is redudant)
* Drop active antenna relay
* Activate target antenna relay
* Wait for relay to settle
* Disable "Transmit Inhibit" on TRX

# HAMLIB vs custom code
* Custom would be faster to react (base on CI-V "transcieve")
* HAMLIB would give me an always available networked CAT, which is handy for many things (WWA & Hamlive for example)

But is fast really that important? By the approach described above, there is never a hot-switch. Worst case is transmitting into the wrong antenna.


