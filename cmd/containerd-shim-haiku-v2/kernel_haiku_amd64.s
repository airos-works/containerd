// Trampolines to the libroot kernel kit calls used by kernel_haiku.go: each
// jumps to the dynamically imported symbol (as the syscall package does).

#include "textflag.h"

TEXT libc__get_next_team_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc__get_next_team_info(SB)
GLOBL	·libc__get_next_team_info_trampoline_addr(SB), RODATA, $8
DATA	·libc__get_next_team_info_trampoline_addr(SB)/8, $libc__get_next_team_info_trampoline<>(SB)

TEXT libc__get_team_usage_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc__get_team_usage_info(SB)
GLOBL	·libc__get_team_usage_info_trampoline_addr(SB), RODATA, $8
DATA	·libc__get_team_usage_info_trampoline_addr(SB)/8, $libc__get_team_usage_info_trampoline<>(SB)

TEXT libc__get_next_area_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc__get_next_area_info(SB)
GLOBL	·libc__get_next_area_info_trampoline_addr(SB), RODATA, $8
DATA	·libc__get_next_area_info_trampoline_addr(SB)/8, $libc__get_next_area_info_trampoline<>(SB)

TEXT libc_sysconf_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sysconf(SB)
GLOBL	·libc_sysconf_trampoline_addr(SB), RODATA, $8
DATA	·libc_sysconf_trampoline_addr(SB)/8, $libc_sysconf_trampoline<>(SB)
