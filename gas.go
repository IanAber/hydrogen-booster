package main

// EstimateTankMassKg-family helpers estimate the mass of hydrogen in a
// known volume from pressure and temperature, since the spec only lists
// pressure/temperature sensors (no mass flow meter). This is used to
// track kg dispensed by watching the storage tank bank empty.
//
// IMPORTANT: this is an approximation intended for operator display and
// as a secondary "target reached" check only. It is NOT calibrated to
// custody-transfer or fuelling-protocol accuracy. A real dispensing
// system delivering hydrogen by mass should use a certified Coriolis
// mass flow meter, and target-pressure fills should follow the
// applicable temperature-compensated fuelling protocol (e.g. SAE J2601 /
// ISO 19880-1) rather than a simple pressure/temperature model. Treat the
// numbers this module produces accordingly.
const (
	hydrogenMolarMassKgPerMol = 0.0020159
	gasConstant               = 8.314462618 // J/(mol*K)
)

func celsiusToKelvin(c float64) float64 { return c + 273.15 }

// zFactorH2 is a coarse empirical fit for hydrogen's compressibility
// factor, adequate only for HMI-level estimates in roughly the 0-800 bar
// range. Unlike most gases, Z for hydrogen rises above 1 with pressure.
// Replace with a proper equation of state (e.g. Leachman et al. / NIST
// REFPROP) for anything safety- or billing-critical.
func zFactorH2(barA float64) float64 {
	z := 1.0 + 0.0005*barA
	if z > 1.45 {
		z = 1.45
	}
	return z
}

// MassInVolumeKg returns the mass of hydrogen (kg) contained in a given
// volume (litres) at the given absolute pressure (bar) and temperature
// (deg C).
func MassInVolumeKg(volumeLitres, pressureBarA, tempC float64) float64 {
	if pressureBarA <= 0 || volumeLitres <= 0 {
		return 0
	}
	pPa := pressureBarA * 1e5
	vM3 := volumeLitres / 1000.0
	tK := celsiusToKelvin(tempC)
	z := zFactorH2(pressureBarA)
	moles := pPa * vM3 / (z * gasConstant * tK)
	return moles * hydrogenMolarMassKgPerMol
}
