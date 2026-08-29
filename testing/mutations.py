#!/usr/bin/env python3
"""Revert each safety guard one at a time and require the suite to fail.

A guard whose removal leaves the tests green is a guard nothing is holding, and
this repo has shipped several: an end-to-end reload test that passed against a
reload path deleted outright, a rollback test that only ever exercised the
success path, a shell scenario that asserted on a directory the run never
touched. Coverage does not catch those. This does.

Two of the entries need a whole-construct replacement rather than a string swap,
and the reason is worth knowing: replacing part of a sort comparator leaves an
INCONSISTENT comparator, and Go may then produce any order — so the result says
nothing about the rule under test. Prepending a plain write to an atomic-write
function leaves the rename in place, so the inode still changes. A mutation that
does not actually remove the behaviour is a false pass.

Run from the repo root:

    python3 testing/mutations.py

Exits non-zero if any guard survives.
"""
import subprocess, sys, shutil, os

# Run from wherever, against the repo this file lives in.
os.chdir(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

# The five allocate budget guards are LAYERED on purpose: removing any one is
# caught by another, so each is listed here for the record and the suite is
# expected to survive them individually. What must not survive is removing them
# together, which is checked separately at the end.
LAYERED = {
    "allocate: the fit precondition ignores the writable pools",
    "allocate: trimToFit returns an over-budget total silently",
    "allocate: no terminal budget assertion",
    "allocate: the per-worker cost is unbounded",
    "allocate: the worker count is unbounded",
    # The recycled waiver has three conditions and any two of them hold the
    # line. `worked` keeps a quiet pool from waiving at all; the counter makes
    # the waiver mean recycling rather than age; and the cold-worker rule stops
    # a waived reading lowering an estimate. Removing all three does not survive,
    # which is checked below.
    "state: the recycled waiver fires on a quiet pool",
    "state: the recycled waiver counts idle samples",
    # The master note is keyed by pool directory in the FILENAME as well as
    # re-checked inside. Either alone keeps a repair off another master.
    "apply: the master note is not keyed by pool directory",
}

# (label, file, old, new)
MUTATIONS = [
    ("allocate: the fit precondition ignores the writable pools",
     "allocate/allocate.go",
     "if unwritableNeed+writableMinimum > allocatable {",
     "if unwritableNeed >= allocatable {"),
    ("allocate: trimToFit returns an over-budget total silently",
     "allocate/allocate.go",
     "			return used, false", "			return used, true"),
    ("allocate: no terminal budget assertion",
     "allocate/allocate.go",
     "	if allocated < 0 || allocated > allocatable {", "	if false {"),
    ("allocate: the per-worker cost is unbounded",
     "allocate/allocate.go",
     "		if p.WorkerBytes > maxPlausibleWorkerBytes {", "		if false {"),
    ("allocate: the worker count is unbounded",
     "allocate/allocate.go",
     "		if p.Floor > maxPlausibleChildren || p.Ceiling > maxPlausibleChildren {",
     "		if false {"),
    ("allocate: the CPU ceiling cuts an unwritable reserve",
     "allocate/allocate.go",
     "		if cpuCap > 0 && floors[i] > cpuCap && !p.Unknown {",
     "		if cpuCap > 0 && floors[i] > cpuCap {"),
    # The WHOLE comparator, not one arm of it: removing a single arm leaves an
    # inconsistent comparator, and Go's sort may then produce any order at all —
    # which makes the result say nothing about the rule under test.
    ("allocate: the queueing pool is served by gap alone",
     "allocate/allocate.go", "@@CMP@@", ""),
    ("allocate: the saturation growth bound is gone",
     "allocate/allocate.go",
     "		if evidence := int(float64(p.ObservedPeak) * opts.HeadroomFactor * opts.GrowthFactor); grown > evidence {\n			grown = evidence\n		}\n",
     ""),
    ("state: the decay gate is a per-scrape count again",
     "state/state.go",
     "	ratio := (float64(served) / since.Seconds()) / opts.MinRequestsPerSecondToDecay",
     "	ratio := float64(0)\n	if served >= 5 {\n		ratio = 1\n	}\n	_ = since"),
    ("state: an unwatched gap counts in full",
     "state/state.go",
     "	effective := cappedSince(ps, since, opts)", "	effective := since\n	_ = cappedSince"),
    ("state: legacy state gets no cadence",
     "state/state.go",
     "	for _, ps := range s.Pools {\n		ps.inferCadence()\n	}\n", ""),
    ("state: the peak is filtered by maturity again",
     "state/state.go",
     "		if w.RSSBytes <= 0 {\n			continue\n		}\n		if w.Requests >= opts.MinRequestsPerWorker {\n			mature++\n		}",
     "		if w.RSSBytes <= 0 || w.Requests < opts.MinRequestsPerWorker {\n			continue\n		}\n		mature++"),
    ("state: confidence accrues without work",
     "state/state.go",
     "	if worked && mature >= opts.MinMatureWorkers {", "	if true {"),
    ("state: a single-worker pool is never measured",
     "state/state.go",
     "	sole := mature == 1 && ps.PeakWorkers <= 1", "	sole := false"),
    ("state: a cold worker lowers the estimate",
     "state/state.go",
     "	if recycled && mature == 0 && peak <= ps.SizingBytes() {",
     "	if false {"),
    ("state: the recycled waiver fires on a quiet pool",
     "state/state.go",
     "	recycled := mature == 0 && worked &&",
     "	recycled := mature == 0 &&"),
    ("state: the recycled waiver is one-shot",
     "state/state.go",
     "	recycled := mature == 0 && worked &&",
     "	recycled := mature == 0 && worked && ps.TypicalPeakBytes == 0 &&"),
    ("apply: the master note is not keyed by pool directory",
     "apply/remembered.go",
     "	if filepath.Clean(ref.DropInDir) != filepath.Clean(dropInDir) {\n		return rememberedMasterRef{}\n	}\n",
     ""),
    ("serve: the daemon plans pools of a master it was not pointed at",
     "serve/serve.go",
     "	return ForMaster(targets, l.cfg.DropInDir, l.log), nil",
     "	return targets, nil"),
    ("cmd: a flag after a positional argument is discarded",
     "cmd/fpm-tune/main.go",
     "	if fs.NArg() == 0 {\n		return nil\n	}\n",
     "	if true {\n		return nil\n	}\n"),
    ("budget: a failed lookup is reported as an unlimited host",
     "budget/budget.go",
     "		limits.LookupErr = err\n\n		return limits",
     "		_ = err\n\n		return limits"),
    ("state: the recycled waiver counts idle samples",
     "state/state.go",
     "		ps.ImmatureRounds >= opts.SamplesBeforeMaturityIsWaived",
     "		ps.Samples >= opts.SamplesBeforeMaturityIsWaived"),
    ("state: a pool is forgotten on its first absence",
     "state/state.go",
     "		if ps.MissedRounds < forgetAfterMissedRounds {", "		if false {"),
    # Whole-body replacement: prepending a WriteFile leaves the rename in place,
    # so the inode still changes and the mutation proves nothing.
    ("state: Save rewrites in place", "state/state.go", "@@BODY@@Save", ""),
    ("plan: an unproven measurement may go below the profile",
     "plan/plan.go",
     "		if ps.BusySamples == 0 && measured < profile.WorkerBytes {", "		if false {"),
    ("plan: the remembered peak is ignored for an unreachable pool",
     "plan/plan.go",
     "		if ps != nil && ps.PeakWorkers > reserve {\n			reserve = ps.PeakWorkers\n		}\n", ""),
    ("plan: an unwritable pool goes through the demand pass",
     "plan/plan.go",
     "			pool.ObservedPeak = 0\n			pool.Ceiling = reserve", "			pool.ObservedPeak = reserve"),
    ("plan: a pool that will not be written still shows a plan number",
     "plan/render.go", "		if p.Unknown {\n			size = \"—\"\n		}\n", ""),
    ("apply: the record is written best-effort",
     "apply/transaction.go", "writeAtomicDurable(transactionPath", "writeAtomic(transactionPath"),
    ("apply: a signalled mismatch is read as never having happened",
     "apply/reconcile.go",
     "	if !txn.landed() && txn.Phase == PhaseWritten {", "	if !txn.landed() {"),
    ("apply: the drop-in is rewritten in place",
     "apply/apply.go",
     "func writeAtomic(path string, content []byte) error {\n	dir := filepath.Dir(path)",
     "func writeAtomic(path string, content []byte) error {\n	return os.WriteFile(path, content, 0o644)\n}\n\nfunc writeAtomicUnused(path string, content []byte) error {\n	dir := filepath.Dir(path)"),
    ("apply: an unsafe pool name is accepted",
     "apply/apply.go",
     "	case strings.ContainsAny(pool, \"\\n\\r\\x00\"):\n		return fmt.Errorf(\"%w: %q contains a control character\", ErrUnsafePoolName, pool)\n",
     ""),
    ("apply: state resurrects overrides after the file is deleted",
     "apply/apply.go", "		_ = st\n	}",
     "		if st != nil {\n			if ps := st.Pools[pp.Name]; ps != nil && ps.LastAppliedMaxChildren > 0 {\n				held := pp\n				held.MaxChildren = ps.LastAppliedMaxChildren\n				held.Reason = \"unchanged\"\n				out = append(out, held)\n			}\n		}\n	}"),
    ("observe: a cancelled discovery reports success",
     "observe/observe.go",
     "	if cerr := ctx.Err(); cerr != nil {\n		return nil, fmt.Errorf(\"discovery was interrupted: %w\", cerr)\n	}\n", ""),
    # MOVED before the plan, not deleted. Deleting it is caught for the wrong
    # reason — the first round never records a counter at all, so the second has
    # nothing to compare against, which is not the bug this names. The bug is
    # the plan comparing a round's own reading against itself.
    ("serve: the counters are recorded before the plan", "serve/serve.go", "@@MOVE@@", ""),
    ("apply: the sandbox validation is bypassed", "apply/apply.go", "@@FUNC@@validateSandboxed", ""),
    ("apply: the live tree is not validated after the write",
     "apply/apply.go",
     "	if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {",
     "	if err := error(nil); err != nil {"),
    ("apply: a rejected leftover is not taken back out",
     "apply/reconcile.go", "@@FUNC@@applyPrevious", ""),
    ("apply: recovery reverts a configuration php-fpm accepts",
     "apply/reconcile.go",
     '			saved := "(none: this was the first apply)"',
     '			previous, perr := previousContent(txn, opts.BackupDir)\n			if perr == nil {\n				_ = applyPrevious(txn, previous)\n			}\n			saved := "(none: this was the first apply)"'),
    ("apply: a rejected leftover with no backup is a dead end",
     "apply/reconcile.go", "@@FUNC@@removeOursIfThatFixesIt", ""),
    ("apply: recovery cannot find php-fpm without a state file",
     "apply/reconcile.go",
     "		master = master.filledFrom(rememberedMaster(opts.BackupDir, master.DropInDir))",
     "		_ = rememberedMaster"),
    ("serve: the lock does not follow the directory being written",
     "serve/serve.go",
     '	if !l.holdResource(master.DropInDir) {\n		return\n	}\n	if !l.reconciled {\n		l.log.Warn("The pool directory changed under this process; reconciling before "+\n			"writing to it", "dir", master.DropInDir)\n\n		return\n	}\n\n	l.metrics.SetApplyBlocked("")',
     '	l.metrics.SetApplyBlocked("")'),
    ("serve: a blocked apply publishes nothing",
     "serve/serve.go", "		l.metrics.SetApplyBlocked(\"no_master\")", "		l.metrics.SetApplyBlocked(\"\")"),
    ("serve: the metrics bind failure is a log line",
     "serve/serve.go",
     "		return nil, fmt.Errorf(\"cannot serve metrics on %s: %w\", l.cfg.MetricsAddr, err)",
     "		l.log.Error(\"no metrics\", \"error\", err)\n\n		return nil, nil"),
]

env = dict(os.environ, GOTOOLCHAIN="go1.26.6")
survived, caught, broken = [], [], []

for label, path, old, new in MUTATIONS:
    src = open(path).read()
    if old.startswith("@@FUNC@@"):
        # Whole function body replaced with a bare return, because these are
        # multi-statement guards and a partial replacement leaves the rest of
        # the body doing half the job.
        name = old[len("@@FUNC@@"):]
        i = src.index("func " + name + "(")
        j = src.index("\n}", src.index("{", i)) + 2
        head = src[i:src.index("{", i) + 1]
        shutil.copy(path, "/tmp/mut.bak")
        open(path, "w").write(src[:i] + head + "\n	return nil\n}\n" + src[j:])
        r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
        shutil.copy("/tmp/mut.bak", path)
        (survived if r.returncode == 0 else caught).append(label)
        print(("SURVIVED     " if r.returncode == 0 else "caught       ") + label)
        continue
    if old == "@@MOVE@@":
        block = ("	// After the plan: the counters are what the NEXT round compares against, and\n"
                 "	// storing them earlier made the comparison one against itself.\n"
                 "	plan.RecordCounters(l.state, views)\n")
        anchor = "	plan.LearnFrom(l.state, views, now, l.cfg.StateOptions)\n"
        moved = src.replace(block, "", 1).replace(
            anchor, anchor + "	plan.RecordCounters(l.state, views)\n", 1)
        shutil.copy(path, "/tmp/mut.bak")
        open(path, "w").write(moved)
        r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
        shutil.copy("/tmp/mut.bak", path)
        (survived if r.returncode == 0 else caught).append(label)
        print(("SURVIVED     " if r.returncode == 0 else "caught       ") + label)
        continue
    if old == "@@CMP@@":
        i = src.index("		sort.SliceStable(cands, func(a, b int) bool {")
        j = src.index("		})", i) + 4
        shutil.copy(path, "/tmp/mut.bak")
        open(path, "w").write(src[:i] +
            "		sort.SliceStable(cands, func(a, b int) bool { return cands[a].gap > cands[b].gap })\n" + src[j:])
        r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
        shutil.copy("/tmp/mut.bak", path)
        (survived if r.returncode == 0 else caught).append(label)
        print(("SURVIVED     " if r.returncode == 0 else "caught       ") + label)
        continue
    if old.startswith("@@BODY@@"):
        name = old[len("@@BODY@@"):]
        i = src.index("func (s *State) " + name + "(")
        i = src.index("	tmp, err := os.CreateTemp(", i)
        j = src.index("\n	return nil\n}", i)
        shutil.copy(path, "/tmp/mut.bak")
        open(path, "w").write(src[:i] + "	if err := os.WriteFile(path, data, 0o644); err != nil {\n		return err\n	}\n" + src[j:])
        r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
        shutil.copy("/tmp/mut.bak", path)
        (survived if r.returncode == 0 else caught).append(label)
        print(("SURVIVED     " if r.returncode == 0 else "caught       ") + label)
        continue
    if old not in src:
        broken.append(label)
        print(f"?? NO MATCH  {label}")
        continue
    shutil.copy(path, "/tmp/mut.bak")
    open(path, "w").write(src.replace(old, new, 1))
    r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
    shutil.copy("/tmp/mut.bak", path)
    if r.returncode == 0:
        survived.append(label)
        print(f"SURVIVED     {label}")
    else:
        caught.append(label)
        print(f"caught       {label}")

# The recycled waiver, all three conditions removed together.
src = open("state/state.go").read()
shutil.copy("state/state.go", "/tmp/mut.bak")
together = src
for o, n in [
    ("	recycled := mature == 0 && worked &&", "	recycled := mature == 0 &&"),
    ("		ps.ImmatureRounds >= opts.SamplesBeforeMaturityIsWaived",
     "		ps.Samples >= opts.SamplesBeforeMaturityIsWaived"),
    ("	if recycled && mature == 0 && peak <= ps.SizingBytes() {", "	if false {"),
]:
    together = together.replace(o, n, 1)
open("state/state.go", "w").write(together)
r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
shutil.copy("/tmp/mut.bak", "state/state.go")
label = "state: every recycled-waiver guard removed together"
(survived if r.returncode == 0 else caught).append(label)
print(("SURVIVED     " if r.returncode == 0 else "caught       ") + label)

# The layers, removed together.
src = open("allocate/allocate.go").read()
shutil.copy("allocate/allocate.go", "/tmp/mut.bak")
together = src
for o, n in [
    ("if unwritableNeed+writableMinimum > allocatable {", "if unwritableNeed >= allocatable {"),
    ("			return used, false", "			return used, true"),
    ("	if allocated < 0 || allocated > allocatable {", "	if false {"),
    ("		if p.WorkerBytes > maxPlausibleWorkerBytes {", "		if false {"),
    ("		if p.Floor > maxPlausibleChildren || p.Ceiling > maxPlausibleChildren {", "		if false {"),
]:
    together = together.replace(o, n, 1)
open("allocate/allocate.go", "w").write(together)
r = subprocess.run(["go", "test", "./allocate/"], capture_output=True, text=True, env=env)
shutil.copy("/tmp/mut.bak", "allocate/allocate.go")
if r.returncode == 0:
    survived.append("allocate: EVERY budget guard removed together")
    print("SURVIVED     allocate: EVERY budget guard removed together")
else:
    caught.append("allocate: every budget guard removed together")
    print("caught       allocate: every budget guard removed together")

survived = [s for s in survived if s not in LAYERED]

print(f"\ncaught {len(caught)}  survived {len(survived)}  unmatched {len(broken)}")
if survived:
    print("\nGuards nothing is holding:")
    for s in survived:
        print("  -", s)
sys.exit(1 if survived or broken else 0)
