/**
 * 外部编辑器与系统定位入口图标组件。
 *
 * Author: fxbin
 */
import { useId } from "react";

/**
 * OpenLocationIconProps 描述打开位置图标渲染参数。
 */
export interface OpenLocationIconProps {
  id?: string;
  size?: number;
  className?: string;
}

/**
 * OpenLocationIcon 按目标位置 ID 渲染对应的应用圆角徽标图标，保持 aria-hidden 不污染按钮可访问名。
 */
export default function OpenLocationIcon({ id, size = 18, className }: OpenLocationIconProps) {
  const uid = useId().replace(/:/g, "");
  const normalized = (id || "default").trim().toLowerCase();
  const cls = className ? `wb-open-loc-icon ${className}` : "wb-open-loc-icon";

  switch (normalized) {
    case "finder": {
      const leftGrad = `finder-l-${uid}`;
      const rightGrad = `finder-r-${uid}`;
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <defs>
              <linearGradient id={leftGrad} x1="0" y1="1" x2="0" y2="19" gradientUnits="userSpaceOnUse">
                <stop offset="0%" stopColor="#35B4FF" />
                <stop offset="100%" stopColor="#1674EA" />
              </linearGradient>
              <linearGradient id={rightGrad} x1="0" y1="1" x2="0" y2="19" gradientUnits="userSpaceOnUse">
                <stop offset="0%" stopColor="#EFF7FF" />
                <stop offset="100%" stopColor="#CBE4FF" />
              </linearGradient>
            </defs>
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill={`url(#${rightGrad})`} />
            <path
              d="M5.5 1H10.9C9.85 5.4 9.45 8.2 9.2 10.7H11.15C10.9 13.4 10.55 16.2 10.15 19H5.5C3.01472 19 1 16.9853 1 14.5V5.5C1 3.01472 3.01472 1 5.5 1Z"
              fill={`url(#${leftGrad})`}
            />
            <rect x="1.35" y="1.35" width="17.3" height="17.3" rx="4.15" stroke="#0E4FA8" strokeOpacity="0.16" strokeWidth="0.7" />
            <path
              d="M10.9 1.6C9.85 5.6 9.45 8.25 9.2 10.7H11.15C10.9 13.35 10.55 16.05 10.18 18.4"
              stroke="#14294A"
              strokeWidth="1.05"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <line x1="6.4" y1="7.1" x2="6.4" y2="9.1" stroke="#14294A" strokeWidth="1.35" strokeLinecap="round" />
            <line x1="13.7" y1="7.1" x2="13.7" y2="9.1" stroke="#14294A" strokeWidth="1.35" strokeLinecap="round" />
            <path
              d="M5.9 13.05C7.15 14.9 8.55 15.65 10.05 15.65C11.55 15.65 12.95 14.9 14.15 13.05"
              stroke="#14294A"
              strokeWidth="1.25"
              strokeLinecap="round"
            />
          </svg>
        </span>
      );
    }

    case "explorer": {
      const folderGrad = `exp-f-${uid}`;
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <defs>
              <linearGradient id={folderGrad} x1="2" y1="3" x2="18" y2="17" gradientUnits="userSpaceOnUse">
                <stop offset="0%" stopColor="#FFE066" />
                <stop offset="100%" stopColor="#F5A623" />
              </linearGradient>
            </defs>
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill="#FFF8E8" />
            <rect x="1.35" y="1.35" width="17.3" height="17.3" rx="4.15" stroke="#D99B26" strokeOpacity="0.24" strokeWidth="0.7" />
            <path
              d="M3.5 6.2C3.5 5.53726 4.03726 5 4.7 5H8.1L9.45 6.45H15.3C15.9627 6.45 16.5 6.98726 16.5 7.65V14.2C16.5 14.8627 15.9627 15.4 15.3 15.4H4.7C4.03726 15.4 3.5 14.8627 3.5 14.2V6.2Z"
              fill={`url(#${folderGrad})`}
            />
            <path d="M6.6 15.4V12.5C6.6 12.0582 6.95817 11.7 7.4 11.7H12.6C13.0418 11.7 13.4 12.0582 13.4 12.5V15.4H6.6Z" fill="#2382E6" />
          </svg>
        </span>
      );
    }

    case "terminal": {
      const termGrad = `term-${uid}`;
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <defs>
              <linearGradient id={termGrad} x1="2" y1="1" x2="18" y2="19" gradientUnits="userSpaceOnUse">
                <stop offset="0%" stopColor="#343942" />
                <stop offset="100%" stopColor="#171A20" />
              </linearGradient>
            </defs>
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill={`url(#${termGrad})`} />
            <rect x="1.4" y="1.4" width="17.2" height="17.2" rx="4.1" stroke="#FFFFFF" strokeOpacity="0.14" strokeWidth="0.8" />
            <path
              d="M5.4 6.9L8.75 9.85L5.4 12.8"
              stroke="#F8FAFC"
              strokeWidth="1.65"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <line x1="10.1" y1="12.9" x2="14.3" y2="12.9" stroke="#F8FAFC" strokeWidth="1.65" strokeLinecap="round" />
          </svg>
        </span>
      );
    }

    case "vscode": {
      const vsGrad = `vsc-${uid}`;
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <defs>
              <linearGradient id={vsGrad} x1="3" y1="3" x2="16" y2="17" gradientUnits="userSpaceOnUse">
                <stop offset="0%" stopColor="#29B6F6" />
                <stop offset="100%" stopColor="#0065A9" />
              </linearGradient>
            </defs>
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill="#EEF6FF" />
            <rect x="1.35" y="1.35" width="17.3" height="17.3" rx="4.15" stroke="#0078D4" strokeOpacity="0.18" strokeWidth="0.7" />
            <path d="M13.65 3.6L7.35 9.65L4.8 7.65L3.65 8.25V11.75L4.8 12.35L7.35 10.35L13.65 16.4L16.15 15.2V4.8L13.65 3.6Z" fill={`url(#${vsGrad})`} />
            <path d="M13.65 3.6V16.4L16.15 15.2V4.8L13.65 3.6Z" fill="#1F9CF0" />
            <path d="M13.65 7.45L9.55 10L13.65 12.55V7.45Z" fill="#EEF6FF" />
          </svg>
        </span>
      );
    }

    case "cursor": {
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill="#141518" />
            <rect x="1.35" y="1.35" width="17.3" height="17.3" rx="4.15" stroke="#FFFFFF" strokeOpacity="0.14" strokeWidth="0.7" />
            {/* 3D 等轴立方体与右上光标切面 */}
            <polygon points="10,3.6 15.5,6.8 10,10 4.5,6.8" fill="#F4F6F9" />
            <polygon points="4.5,6.8 10,10 10,16.4 4.5,13.2" fill="#9DA5B4" />
            <polygon points="15.5,6.8 15.5,13.2 10,16.4 10,10" fill="#596170" />
            <polygon points="10,6.8 15.5,6.8 10,10" fill="#FFFFFF" />
          </svg>
        </span>
      );
    }

    case "idea": {
      const ideaGrad = `idea-${uid}`;
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <defs>
              <linearGradient id={ideaGrad} x1="1" y1="1" x2="19" y2="19" gradientUnits="userSpaceOnUse">
                <stop offset="0%" stopColor="#F97A12" />
                <stop offset="48%" stopColor="#FE315D" />
                <stop offset="100%" stopColor="#087CFA" />
              </linearGradient>
            </defs>
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill={`url(#${ideaGrad})`} />
            <rect x="3.2" y="3.2" width="13.6" height="13.6" rx="2.4" fill="#14151A" />
            <path
              d="M5.7 6.3H8.1M6.9 6.3V10.8M5.7 10.8H8.1"
              stroke="#FFFFFF"
              strokeWidth="1.3"
              strokeLinecap="round"
            />
            <path
              d="M11.9 6.3V9.65C11.9 10.45 11.4 10.9 10.65 10.9C10.1 10.9 9.75 10.65 9.55 10.25"
              stroke="#FFFFFF"
              strokeWidth="1.3"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <line x1="5.6" y1="13.85" x2="10.2" y2="13.85" stroke="#FFFFFF" strokeWidth="1.35" strokeLinecap="round" />
          </svg>
        </span>
      );
    }

    case "ghostty": {
      const ghostGrad = `ghost-${uid}`;
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <defs>
              <linearGradient id={ghostGrad} x1="2" y1="1" x2="18" y2="19" gradientUnits="userSpaceOnUse">
                <stop offset="0%" stopColor="#2E3B82" />
                <stop offset="100%" stopColor="#161C42" />
              </linearGradient>
            </defs>
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill={`url(#${ghostGrad})`} />
            <rect x="1.35" y="1.35" width="17.3" height="17.3" rx="4.15" stroke="#8FA4FF" strokeOpacity="0.22" strokeWidth="0.7" />
            <path
              d="M5.6 9.4C5.6 6.75 7.55 4.7 10 4.7C12.45 4.7 14.4 6.75 14.4 9.4V14.85L12.95 13.75L11.5 14.85L10 13.75L8.5 14.85L7.05 13.75L5.6 14.85V9.4Z"
              fill="#F8FAFF"
            />
            <path
              d="M7.65 8.7L9.05 9.75L7.65 10.8"
              stroke="#1B2250"
              strokeWidth="1.25"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <line x1="10.25" y1="10.8" x2="12.2" y2="10.8" stroke="#1B2250" strokeWidth="1.25" strokeLinecap="round" />
          </svg>
        </span>
      );
    }

    default: {
      return (
        <span className={cls} aria-hidden="true">
          <svg width={size} height={size} viewBox="0 0 20 20" fill="none">
            <rect x="1" y="1" width="18" height="18" rx="4.5" fill="var(--wb-elev)" />
            <rect x="1.4" y="1.4" width="17.2" height="17.2" rx="4.1" stroke="var(--wb-line-strong)" strokeWidth="0.9" />
            <path
              d="M8.1 7.4L5.6 10L8.1 12.6M11.9 7.4L14.4 10L11.9 12.6"
              stroke="currentColor"
              strokeWidth="1.45"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </span>
      );
    }
  }
}
