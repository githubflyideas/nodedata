package acceptance

import (
	"testing"
	"time"
	"github.com/githubflyideas/nodedata/internal/collector"
)

func TestT_PARSE_01(t *testing.T) { T_PARSE_01_GoldenSnapshot(t) }
func TestT_PARSE_02(t *testing.T) {
    // Fresh collector so prevTS is truly Zero
    old := col
    col = &adaptedCollector{collector.New(collector.Config{Interval: 30 * time.Second})}
    T_PARSE_02_CounterFirstRoundDropped(t)
    col = old
}
func TestT_PARSE_03(t *testing.T) { T_PARSE_03_CounterWrapNoNegative(t) }
func TestT_PARSE_04(t *testing.T) { T_PARSE_04_DerivedZeroDenominator(t) }
func TestT_PARSE_05(t *testing.T) { T_PARSE_05_MalformedInputNoPanic(t) }
func TestT_PARSE_06(t *testing.T) { T_PARSE_06_MultiDeviceAggregation(t) }
func TestT_SAFE_01(t *testing.T)  { T_SAFE_01_ForbiddenPaths(t) }
func TestT_SAFE_02(t *testing.T)  { T_SAFE_02_ConntrackViaSysctl(t) }
func TestT_SAFE_03(t *testing.T) {
    // Fresh collector whose psiAvailable is set based on no_psi procRoot
    old := col
    col = &adaptedCollector{collector.New(collector.Config{ProcRoot: "testdata/procfs/no_psi", Interval: 30 * time.Second})}
    T_SAFE_03_PSIMissingDegrades(t)
    col = old
}
func TestT_SAFE_04(t *testing.T)  { T_SAFE_04_NoCapNetAdminStarts(t) }
func TestT_SAFE_05(t *testing.T)  { T_SAFE_05_ProcSamplingUnderLoad(t) }
func TestT_SAFE_06(t *testing.T)  { T_SAFE_06_BudgetOverrunDowngrades(t) }
func TestT_LAG_01(t *testing.T)   { T_LAG_01_SigmaComputation(t) }
func TestT_LAG_02(t *testing.T)   { T_LAG_02_SigmaFloor(t) }
func TestT_LAG_03(t *testing.T)   { T_LAG_03_OnsetLocalization(t) }
func TestT_LAG_04(t *testing.T)   { T_LAG_04_SigmaBucketedByHour(t) }
func TestT_LAG_05(t *testing.T)   { T_LAG_05_AlignedLagsAreDayMultiples(t) }
func TestT_LAG_06(t *testing.T)   { T_LAG_06_ConstantFaultBlindSpot(t) }
func TestT_LAG_07(t *testing.T)   { T_LAG_07_DiurnalNoFalsePositive(t) }
func TestT_LAG_08(t *testing.T)   { T_LAG_08_ColdStart(t) }
func TestT_LAG_09(t *testing.T)   { T_LAG_09_LowConfidenceFlag(t) }
func TestT_LAG_10(t *testing.T)   { T_LAG_10_Classification(t) }
func TestT_LAG_11(t *testing.T)   { T_LAG_11_SigmaTableSizeAndCadence(t) }
func TestT_LAG_12(t *testing.T)   { T_LAG_12_LookupNearestNoInterpolation(t) }
func TestT_LAG_13(t *testing.T)   { T_LAG_13_SigmaSurvivesIntervalChange(t) }
func TestT_PERF_01(t *testing.T)  { T_PERF_01_GlobalCollectLatency(t) }
func TestT_PERF_02(t *testing.T)  { T_PERF_02_ZeroAllocCollect(t) }
func TestT_PERF_03(t *testing.T)  { T_PERF_03_StaticDump(t) }
func TestT_PERF_06(t *testing.T)  { T_PERF_06_AtomicDump(t) }
func TestT_E2E_01(t *testing.T)   { T_E2E_01_SingleDomainFault(t) }
func TestT_E2E_02(t *testing.T)   { T_E2E_02_CascadeDetection(t) }
func TestT_E2E_03(t *testing.T)   { T_E2E_03_ChronicDriftDetection(t) }
func TestT_E2E_04(t *testing.T)   { T_E2E_04_FaultVsBenignDiscrimination(t) }
func TestT_E2E_05(t *testing.T)   { T_E2E_05_PeriodicNotAlerted(t) }
func TestT_E2E_06(t *testing.T)   { T_E2E_06_NoFaultHiddenAsPeriodic(t) }
