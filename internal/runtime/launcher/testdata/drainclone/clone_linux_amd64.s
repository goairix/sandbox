#include "textflag.h"

// The parent masks signals only around clone and restores its original mask on
// every parent/error path. The child never returns or accesses the Go runtime.
TEXT ·rawClone(SB),NOSPLIT,$24-16
 MOVQ $-1, 0(SP)
 MOVQ $14, AX // rt_sigprocmask(SIG_SETMASK, all, old, 8)
 MOVQ $2, DI
 LEAQ 0(SP), SI
 LEAQ 8(SP), DX
 MOVQ $8, R10
 SYSCALL
 TESTQ AX, AX
 JNZ maskFailed
 MOVQ flags+0(FP), DI
 XORQ SI, SI
 XORQ DX, DX
 XORQ R10, R10
 XORQ R8, R8
 MOVQ $56, AX // clone(flags, NULL, NULL, NULL, 0)
 SYSCALL
 TESTQ AX, AX
 JZ child
 MOVQ AX, 16(SP)
 MOVQ $14, AX
 MOVQ $2, DI
 LEAQ 8(SP), SI
 XORQ DX, DX
 MOVQ $8, R10
 SYSCALL
 TESTQ AX, AX
 JNZ maskFailed
 MOVQ 16(SP), AX
maskFailed:
 MOVQ AX, ret+8(FP)
 RET
child:
 XORQ DI, DI
 XORQ SI, SI
 XORQ DX, DX
 XORQ R10, R10
 XORQ R8, R8
 MOVQ $271, AX // ppoll(NULL,0,NULL,NULL,0); SIGKILL is still deliverable
 SYSCALL
 JMP child
