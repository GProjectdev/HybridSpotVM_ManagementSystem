# Withdrawn: custom Karmada Work status patch

Do not build or deploy the earlier custom Karmada controller-manager image.
The project keeps upstream Karmada unchanged. The integration fix belongs in
PV-Migration-System and the System restore gate.

Use [E1 split Work release v5.0.5](e1-split-work-v5.0.5.md).
It emits separate PV and PVC Works while retaining reciprocal binding checks.
If the withdrawn image was already installed, restore the original deployment
from its backup before validating this release. Never bypass volume checks.
