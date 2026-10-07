#include "textflag.h"

// Locals follow the saved LR. No child branch can RET or call Go code.
TEXT ·rawClone(SB),NOSPLIT,$32-16
 MOVD $-1, R0
 MOVD R0, 8(RSP)
 MOVD $2, R0
 ADD $8, RSP, R1
 ADD $16, RSP, R2
 MOVD $8, R3
 MOVD $135, R8 // rt_sigprocmask(SIG_SETMASK, all, old, 8)
 SVC
 CBNZ R0, maskFailed
 MOVD flags+0(FP), R0
 MOVD ZR, R1
 MOVD ZR, R2
 MOVD ZR, R3
 MOVD ZR, R4
 MOVD $220, R8 // clone(flags,NULL,NULL,0,NULL)
 SVC
 CBZ R0, child
 MOVD R0, 24(RSP)
 MOVD $2, R0
 ADD $16, RSP, R1
 MOVD ZR, R2
 MOVD $8, R3
 MOVD $135, R8
 SVC
 CBNZ R0, maskFailed
 MOVD 24(RSP), R0
maskFailed:
 MOVD R0, ret+8(FP)
 RET
child:
 MOVD ZR, R0
 MOVD ZR, R1
 MOVD ZR, R2
 MOVD ZR, R3
 MOVD ZR, R4
 MOVD $73, R8 // ppoll(NULL,0,NULL,NULL,0)
 SVC
 B child
