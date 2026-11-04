# Scheduled E2 interruption

Use a fresh run after cleaning up the manual pilot. Do not include the manual
pilot in evaluation results. Do not remove its ownership files before cleanup.

Run the existing E2 render, prepare and start commands in terminal A. Prepare a
new instance-scoped FIS role and an On-Demand health alarm for this run; previous
instance IDs and alarm dimensions must not be reused.

In terminal B, with the same kubeconfig variables and run directory:

```bash
python3 -u evaluation/e2_schedule.py --evidence "${EVIDENCE:?}" \
  --confirm "${RUN:?}" --instance-id "${SPOT_INSTANCE_ID:?}" \
  --role-arn "${FIS_ROLE_ARN:?}" --alarm-arn "${FIS_ALARM_ARN:?}"
```

This foreground process must remain alive. It uses the later of the first
recorded step timestamps from rank 0 and rank 1, not VM boot time. Host and
training clocks must be synchronized. It requests FIS 1200 seconds after that
anchor. The existing FIS action then gives a two-minute interruption warning;
the VM is not promised to terminate at the 20-minute mark.

At the deadline the original owned Spot identity, durable checkpoint and exact
alarm ARN in OK state are checked. Failure cancels injection rather than retrying
at an uncontrolled time. A deadline already passed or more than 30 seconds late
is rejected. Exclusive fis-schedule.json creation prevents duplicate schedulers.
Do not also use manual interrupt. Do not delete the schedule or fis-started.json
to retry an uncertain request. Ctrl+C in terminal B cancels waiting, not an
already submitted interruption. Ctrl+C in terminal A alone does not stop B
immediately; stop B first when cancelling.

Keep fis-schedule.json with the new run evidence; it is not currently included
in the presentation archive. The existing runner timeline records the actual
FIS request. Successful submission is not proof of successful recovery.
