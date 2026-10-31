/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

const consoleDevice = "ttyAMA0"

// Under TCG, no kernel page table isolation: KASLR turns it on for cores
// without E0PD (neoverse-n1 among them), and each switch between the two
// page tables flushes QEMU's software TLB, which made a system call cost
// seven times as much. An emulated core does not speculate, so there is no
// Meltdown for KPTI to guard against.
func kernelArgs(accel string) string {
	if accel == "tcg" {
		return " kpti=off"
	}
	return ""
}

func machineType(accel string) string {
	if accel == "tcg" {
		return "virt,gic-version=max"
	}
	return "virt"
}

// Under TCG, an ARMv8.2 core with LSE atomics but no SVE, SME or MTE: their
// state costs every context switch, and nothing in the VM needs them.
func cpuModel(accel string) string {
	if accel == "tcg" {
		return "neoverse-n1"
	}
	return "host"
}

func virtioDevice(name string) string { return name + "-pci" }

func goarch() string { return "arm64" }
