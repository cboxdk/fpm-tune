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

Every mutation is applied to the REAL working tree and undone afterwards. That
is what makes it honest — it runs the tests you actually have against the code
you actually shipped — and it is also its one hazard: an interruption between
applying and restoring leaves a safety guard removed. One was, by a timeout
while this was being written, and it was nearly committed. So the restore is
registered the moment a file is touched and runs on exit, exception, Ctrl-C and
kill alike. Check `git status` after an interrupted run anyway.

Run from the repo root:

    python3 testing/mutations.py

Exits non-zero if any guard survives, or if a mutation no longer matches.
"""
import atexit
import os
import shutil
import signal
import subprocess
import sys

# Every mutation is applied to the real working tree, so an interruption between
# applying one and restoring it leaves the repo mutated — and a mutation is by
# construction a safety guard removed. One was left behind by a timeout while
# this was being written, and it was nearly committed: the daemon silently
# stopped publishing why it could not apply.
#
# So the restore is registered the moment a file is touched, and runs on a
# normal exit, an exception, Ctrl-C and a kill alike.
_pending = {}


def _restore_all(*_):
    for target, backup in list(_pending.items()):
        shutil.copy(backup, target)
        _pending.pop(target, None)


atexit.register(_restore_all)
for _sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    signal.signal(_sig, lambda s, f: (_restore_all(), sys.exit(130)))


def stash(path):
    """Back a file up and arrange for it to come back whatever happens."""
    backup = "/tmp/mut-" + path.replace("/", "_") + ".bak"
    shutil.copy(path, backup)
    _pending[path] = backup

    return backup


def unstash(path):
    backup = _pending.pop(path, None)
    if backup:
        shutil.copy(backup, path)

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
    "state: a low-rate recycling pool is never measured",
    "state: a low-rate recycling pool never earns the waiver",
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
     "state/state.go", "		ps.inferCadence()\n", ""),
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
    ("state: the recycled waiver is one-shot",
     "state/state.go",
     "	recycled := mature == 0 && servedSomething &&",
     "	recycled := mature == 0 && servedSomething && ps.TypicalPeakBytes == 0 &&"),
    ("state: a low-rate recycling pool is never measured",
     "state/state.go",
     "	recycled := mature == 0 && servedSomething &&",
     "	recycled := mature == 0 && worked &&"),
    ("state: a low-rate recycling pool never earns the waiver",
     "state/state.go",
     "	if mature == 0 && len(obs.Workers) > 0 && servedSomething {",
     "	if mature == 0 && len(obs.Workers) > 0 && worked {"),
    ("plan: one old record is reserved for every pool of that name",
     "plan/plan.go",
     "		if ambiguous {\n			// No legacy fallback: the old unscoped record belongs to at most one\n			// of the pools sharing this name, and giving it to both reserves the\n			// same history twice.\n			ps = st.LookupScoped(view.Target.ConfigPath, view.Name)\n		} else {\n			ps = st.Lookup(view.Target.ConfigPath, view.Name)\n		}",
     "		ps = st.Lookup(view.Target.ConfigPath, view.Name)"),
    ("allocate: the cheapest urgent fix is counted in workers",
     "allocate/allocate.go",
     "				return int64(cands[a].gap)*pools[cands[a].i].WorkerBytes <\n					int64(cands[b].gap)*pools[cands[b].i].WorkerBytes",
     "				return cands[a].gap < cands[b].gap"),
    ("cmd: apply writes from a budget nobody confirmed",
     "cmd/fpm-tune/main.go",
     "	if limits.LookupErr == nil {\n		return nil\n	}",
     "	if true {\n		return nil\n	}"),
    ("serve: the daemon keeps the lock it has stopped writing with",
     "serve/serve.go", "		l.releaseResource()\n", ""),
    ("serve: the daemon applies from a budget nobody confirmed",
     "serve/serve.go",
     "	if result.Budget.LookupErr != nil {\n		l.metrics.SetApplyBlocked(\"budget_unconfirmed\")",
     "	if false {\n		l.metrics.SetApplyBlocked(\"budget_unconfirmed\")"),
    ("state: two masters' pools of the same name share one record",
     "state/state.go",
     '	if master == "" {\n		// Legacy, and the unscoped case: a single-master host, or a record\n		// written before this existed.\n		return pool\n	}\n\n	return master + "::" + pool',
     "	return pool"),
    # NOT listed: the backup step's refusal to write over an unreadable file.
    # It is layered behind parseOurs, which reaches the file first and refuses
    # the run — so removing the later branch changes nothing a test can see. It
    # exists for the window between the two, which no test can stage, and
    # listing it here would report a survivor every run and teach people to
    # ignore the output.
    ("state: a scoped round resets another master's absence counter",
     "state/state.go", "@@SCOPEORDER@@", ""),
    ("serve: an unconfirmed reload advances the last-apply timestamp",
     "serve/serve.go",
     "	l.metrics.RecordApply(float64(now.Unix()), applied.Wrote && !applied.Inconclusive,",
     "	l.metrics.RecordApply(float64(now.Unix()), applied.Wrote,"),
    ("cmd: a failed save after a successful apply is a warning",
     "cmd/fpm-tune/main.go",
     "	if err == nil {\n		return nil\n	}\n\n	return fmt.Errorf(\"the configuration was applied",
     "	if true {\n		return nil\n	}\n\n	return fmt.Errorf(\"the configuration was applied"),
    ("metrics: ambiguous pool names are published anyway",
     "metrics/metrics.go", "		if ambiguous[p.Name] {\n			continue\n		}\n\n", ""),
    ("state: a scoped daemon forgets another master's pools",
     "state/state.go",
     '		if scope != "" && ps.MasterConfig != scope {\n			continue\n		}\n', ""),
    ("state: a clock correction releases the reload brake",
     "state/state.go",
     "	if ps.LastAppliedAt.After(horizon) {\n		ps.LastAppliedAt = now\n	}",
     "	if ps.LastAppliedAt.After(horizon) {\n		ps.LastAppliedAt = time.Time{}\n	}"),
    ("serve: a remembered master is reused for another directory",
     "serve/serve.go",
     '		if dropInDir != "" && filepath.Clean(remembered.DropInDir) != filepath.Clean(dropInDir) {\n			return apply.Master{}, ErrNoMaster\n		}\n', ""),
    ("apply: the master note is not keyed by pool directory",
     "apply/remembered.go",
     "	if filepath.Clean(ref.DropInDir) != filepath.Clean(dropInDir) {\n		return rememberedMasterRef{}\n	}\n",
     ""),
    ("serve: the daemon plans pools of a master it was not pointed at",
     "serve/serve.go",
     "	return ForMaster(targets, l.cfg.DropInDir, l.log), nil",
     "	return targets, nil"),
    ("cmd: a failed apply still prints a table of things it did",
     "cmd/fpm-tune/main.go",
     '	if err != nil || res.RolledBack || dryRun {\n		heading = "WOULD"\n	}',
     "	if false {\n		heading = \"WOULD\"\n	}"),
    ("serve: -no-learn still forgets",
     "serve/serve.go", "	if !l.cfg.NoLearn {\n		if dropped := l.state.Forget(",
     "	if true {\n		if dropped := l.state.Forget("),
    ("serve: -no-learn saves on the way out",
     "serve/serve.go",
     "	if l.cfg.NoLearn {\n		// The one save that went straight to the store",
     "	if false {\n		// The one save that went straight to the store"),
    ("budget: a host with no /proc reports a failed lookup",
     "budget/budget.go", "	if _, err := os.Stat(procRoot); err != nil {", "	if false {"),
    ("budget: an unreadable limit file reads as no limit",
     "budget/budget.go",
     "				if rerr != nil {\n					// The limit exists and cannot be read, which is the case\n					// that must not pass for \"unlimited\".\n					return 0, false, fmt.Errorf(\"cannot read the memory limit at %s: %w\",\n						filepath.Join(base, path, file), rerr)\n				}\n",
     "				_ = rerr\n"),
    ("serve: -no-learn records anyway",
     "serve/serve.go",
     "	if !l.cfg.NoLearn {\n		plan.LearnFrom(l.state, views, now, l.cfg.StateOptions)\n	}",
     "	plan.LearnFrom(l.state, views, now, l.cfg.StateOptions)"),
    ("state: a save widens the operator's chosen permissions",
     "state/state.go",
     "	mode := os.FileMode(0o644)\n	if info, err := os.Stat(path); err == nil {\n		mode = info.Mode().Perm()\n	}",
     "	mode := os.FileMode(0o644)"),
    ("state: a state file from the future is believed",
     "state/state.go", "		ps.forgetTheFuture(now)", "		_ = now"),
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
    # Through Lookup, not the bare map: applied state is stored under the
    # (master, pool) key now, so a mutant reading st.Pools[name] only ever
    # resurrects a legacy record and the test would pass for the wrong reason.
    ("apply: state resurrects overrides after the file is deleted",
     "apply/apply.go", "		_ = st\n	}",
     "		if st != nil {\n			if ps := st.Lookup(master.ConfigPath, pp.Name); ps != nil && ps.LastAppliedMaxChildren > 0 {\n				held := pp\n				held.MaxChildren = ps.LastAppliedMaxChildren\n				held.Reason = \"unchanged\"\n				out = append(out, held)\n			}\n		}\n	}"),
    ("observe: the view takes whatever pool the map yields",
     "observe/observe.go",
     "	pool, ok := outcome.Result.Pools[outcome.Name]",
     "	var pool phpfpm.Pool\n	var ok bool\n	for _, p := range outcome.Result.Pools {\n		pool, ok = p, true\n\n		break\n	}"),
    ("observe: a pool with no result loses its ceiling",
     "observe/observe.go",
     "	if target.MaxChildren > 0 {\n		view.CurrentMaxChildren, view.MaxChildrenKnown = target.MaxChildren, true\n	}\n	if target.ProcessManager != \"\" {\n		view.ProcessManager = target.ProcessManager\n	}\n",
     ""),
    ("budget: the soft ceiling is ignored",
     "budget/budget.go",
     '			base, files = cgroupRoot, []string{"memory.max", "memory.high"}',
     '			base, files = cgroupRoot, []string{"memory.max"}'),
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
     '	if !l.holdResource(master.DropInDir) {\n		return\n	}\n	if !l.reconciled {\n		l.log.Warn("The pool directory changed under this process; reconciling before "+\n			"writing to it", "dir", master.DropInDir)\n\n		return\n	}\n',
     ""),
    ("serve: a blocked apply publishes nothing",
     "serve/serve.go", "		l.metrics.SetApplyBlocked(\"no_master\")", "		l.metrics.SetApplyBlocked(\"\")"),
    ("serve: the metrics bind failure is a log line",
     "serve/serve.go",
     "		return nil, fmt.Errorf(\"cannot serve metrics on %s: %w\", l.cfg.MetricsAddr, err)",
     "		l.log.Error(\"no metrics\", \"error\", err)\n\n		return nil, nil"),
]

env = dict(os.environ, GOTOOLCHAIN="go1.26.6")


def suite_fails(path):
    """Run the tests and report whether the mutation in `path` was caught.

    The mutated package's OWN tests first, with -failfast: most guards are held
    by a test beside them, and stopping there turns a full-suite run into a
    fraction of one. Catching is catching, whoever catches it.

    Only when its own package stays green does the whole suite run, because the
    interesting mutations are exactly the ones another package holds — state's
    rules are caught by plan's tests, serve's ordering by its own, and the
    allocator's layers by a sweep three packages away.
    """
    pkg = "./" + os.path.dirname(path) if os.path.dirname(path) else "./..."
    if pkg != "./...":
        r = subprocess.run(["go", "test", "-failfast", "-count=1", pkg],
                           capture_output=True, text=True, env=env)
        if r.returncode != 0:
            return True

    r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)

    return r.returncode != 0


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
        stash(path)
        open(path, "w").write(src[:i] + head + "\n	return nil\n}\n" + src[j:])
        failed = suite_fails(path)
        unstash(path)
        (caught if failed else survived).append(label)
        print(("caught       " if failed else "SURVIVED     ") + label)
        continue
    if old == "@@MOVE@@":
        block = ("	// After the plan: the counters are what the NEXT round compares against, and\n"
                 "	// storing them earlier made the comparison one against itself.\n"
                 "	plan.RecordCounters(l.state, views)\n")
        anchor = "	plan.LearnFrom(l.state, views, now, l.cfg.StateOptions)\n"
        moved = src.replace(block, "", 1).replace(
            anchor, anchor + "	plan.RecordCounters(l.state, views)\n", 1)
        stash(path)
        open(path, "w").write(moved)
        failed = suite_fails(path)
        unstash(path)
        (caught if failed else survived).append(label)
        print(("caught       " if failed else "SURVIVED     ") + label)
        continue
    if old == "@@SCOPEORDER@@":
        # The guard MOVED back after the keep check, which is where it was.
        guard = ('		if scope != "" && ps.MasterConfig != scope {\n'
                 "			continue\n		}\n\n")
        i = src.index(guard + "		if keep[ps.Pool] {")
        moved = src[:i] + src[i + len(guard):]
        j = moved.index("		name := key\n", i) + len("		name := key\n")
        stash(path)
        open(path, "w").write(moved[:j] + "\n" + guard + moved[j:])
        failed = suite_fails(path)
        unstash(path)
        (caught if failed else survived).append(label)
        print(("caught       " if failed else "SURVIVED     ") + label)
        continue
    if old == "@@CMP@@":
        i = src.index("		sort.SliceStable(cands, func(a, b int) bool {")
        j = src.index("		})", i) + 4
        stash(path)
        open(path, "w").write(src[:i] +
            "		sort.SliceStable(cands, func(a, b int) bool { return cands[a].gap > cands[b].gap })\n" + src[j:])
        failed = suite_fails(path)
        unstash(path)
        (caught if failed else survived).append(label)
        print(("caught       " if failed else "SURVIVED     ") + label)
        continue
    if old.startswith("@@BODY@@"):
        name = old[len("@@BODY@@"):]
        i = src.index("func (s *State) " + name + "(")
        i = src.index("	tmp, err := os.CreateTemp(", i)
        j = src.index("\n	return nil\n}", i)
        stash(path)
        open(path, "w").write(src[:i] + "	if err := os.WriteFile(path, data, 0o644); err != nil {\n		return err\n	}\n" + src[j:])
        failed = suite_fails(path)
        unstash(path)
        (caught if failed else survived).append(label)
        print(("caught       " if failed else "SURVIVED     ") + label)
        continue
    if old not in src:
        broken.append(label)
        print(f"?? NO MATCH  {label}")
        continue
    stash(path)
    open(path, "w").write(src.replace(old, new, 1))
    failed = suite_fails(path)
    unstash(path)
    if failed:
        caught.append(label)
        print(f"caught       {label}")
    else:
        survived.append(label)
        print(f"SURVIVED     {label}")

# The master note's two layers, removed together: the keyed filename and the
# payload re-check. Either alone keeps a repair off another master.
src = open("apply/remembered.go").read()
stash("apply/remembered.go")
together = src
for o, n in [
    ('	return hex.EncodeToString(sum[:4]) + "-master.json"', '	_ = sum\n\n	return "master.json"'),
    ('	if filepath.Clean(ref.DropInDir) != filepath.Clean(dropInDir) {\n		return rememberedMasterRef{}\n	}\n', ""),
]:
    together = together.replace(o, n, 1)
open("apply/remembered.go", "w").write(together)
r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
unstash("apply/remembered.go")
label = "apply: both master-note layers removed together"
(survived if r.returncode == 0 else caught).append(label)
print(("SURVIVED     " if r.returncode == 0 else "caught       ") + label)

# The recycled waiver, all three conditions removed together.
src = open("state/state.go").read()
stash("state/state.go")
together = src
for o, n in [
    ("	recycled := mature == 0 && servedSomething &&", "	recycled := mature == 0 &&"),
    ("		ps.ImmatureRounds >= opts.SamplesBeforeMaturityIsWaived",
     "		ps.Samples >= opts.SamplesBeforeMaturityIsWaived"),
    ("	if recycled && mature == 0 && peak <= ps.SizingBytes() {", "	if false {"),
]:
    together = together.replace(o, n, 1)
open("state/state.go", "w").write(together)
r = subprocess.run(["go", "test", "./..."], capture_output=True, text=True, env=env)
unstash("state/state.go")
label = "state: every recycled-waiver guard removed together"
(survived if r.returncode == 0 else caught).append(label)
print(("SURVIVED     " if r.returncode == 0 else "caught       ") + label)

# The layers, removed together.
src = open("allocate/allocate.go").read()
stash("allocate/allocate.go")
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
unstash("allocate/allocate.go")
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
