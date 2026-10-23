// BudgetContextForm 预算/上下文编辑表单；提交走 config 写路径。
// Author: fxbin
import { useState } from "react";

export interface BudgetContextValues {
  maxTurns: number;
  maxInputTokens: number;
  maxOutputTokens: number;
  autoCompact: boolean;
  maxPromptTokens: number;
  maxSummaryBytes: number;
}

interface Props {
  initial: BudgetContextValues;
  onSubmit: (values: BudgetContextValues) => void;
}

export function BudgetContextForm({ initial, onSubmit }: Props) {
  const [values, setValues] = useState(initial);
  const update = (patch: Partial<BudgetContextValues>) =>
    setValues({ ...values, ...patch });
  return (
    <div data-testid="budget-context-form" className="field">
      <span>预算与上下文</span>
      <label>
        最大轮次
        <input
          data-testid="budget-max-turns"
          type="number"
          value={values.maxTurns}
          onChange={(e) => update({ maxTurns: Number(e.target.value) })}
        />
      </label>
      <label>
        最大输入 token
        <input
          data-testid="budget-max-input"
          type="number"
          value={values.maxInputTokens}
          onChange={(e) => update({ maxInputTokens: Number(e.target.value) })}
        />
      </label>
      <label>
        最大输出 token
        <input
          data-testid="budget-max-output"
          type="number"
          value={values.maxOutputTokens}
          onChange={(e) => update({ maxOutputTokens: Number(e.target.value) })}
        />
      </label>
      <label>
        自动压缩
        <input
          data-testid="context-auto-compact"
          type="checkbox"
          checked={values.autoCompact}
          onChange={(e) => update({ autoCompact: e.target.checked })}
        />
      </label>
      <label>
        上下文上限
        <input
          data-testid="context-max-prompt"
          type="number"
          value={values.maxPromptTokens}
          onChange={(e) => update({ maxPromptTokens: Number(e.target.value) })}
        />
      </label>
      <button
        data-testid="budget-context-submit"
        type="button"
        onClick={() => onSubmit(values)}
      >
        保存
      </button>
    </div>
  );
}
