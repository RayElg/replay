interface IconProps {
  d: string | string[]
  fill?: string
  strokeWidth?: number
  size?: number
  className?: string
}

function Icon({ d, fill, strokeWidth = 1.6, size = 16, className }: IconProps) {
  return (
    <svg viewBox="0 0 16 16" width={size} height={size} fill={fill ?? 'none'}
         stroke="currentColor" strokeWidth={strokeWidth}
         strokeLinecap="round" strokeLinejoin="round" className={className}>
      {Array.isArray(d)
        ? d.map((p, i) => <path key={i} d={p} />)
        : <path d={d} />}
    </svg>
  )
}

export const Icons = {
  search:    () => <Icon d="M11.5 11.5 14 14 M7 12.5a5.5 5.5 0 1 0 0-11 5.5 5.5 0 0 0 0 11Z" />,
  play:      () => <Icon d="M4 3v10l9-5-9-5Z" fill="currentColor" strokeWidth={0} />,
  pause:     () => <Icon d="M5 3v10 M11 3v10" />,
  chevR:     () => <Icon d="M6 4l4 4-4 4" />,
  chevL:     () => <Icon d="M10 4 6 8l4 4" />,
  check:     () => <Icon d="M3.5 8.5 6.5 11.5l6-6" />,
  x:         () => <Icon d="M4 4l8 8 M12 4l-8 8" />,
  warn:      () => <Icon d="M8 2.5 14.5 13H1.5L8 2.5Z M8 7v3 M8 12v.01" />,
  retry:     () => <Icon d={["M13 4v3h-3", "M13 7A5 5 0 0 0 3.7 9"]} />,
  zap:       () => <Icon d="M9 1 3 9h4l-1 6 7-9H9l1-5Z" />,
  github:    () => <Icon d="M8 1.5a6.5 6.5 0 0 0-2.05 12.66c.32.06.44-.14.44-.31v-1.13c-1.81.4-2.19-.87-2.19-.87-.3-.76-.73-.96-.73-.96-.6-.41.05-.4.05-.4.66.05 1 .68 1 .68.59 1 1.54.71 1.92.55.06-.43.23-.71.42-.88-1.45-.16-2.97-.72-2.97-3.22 0-.71.25-1.29.67-1.75-.07-.16-.29-.83.06-1.73 0 0 .55-.18 1.81.67a6.3 6.3 0 0 1 3.3 0c1.25-.85 1.81-.67 1.81-.67.35.9.13 1.57.06 1.73.42.46.67 1.04.67 1.75 0 2.5-1.52 3.06-2.97 3.22.23.2.44.59.44 1.19v1.77c0 .17.11.38.45.31A6.5 6.5 0 0 0 8 1.5Z" fill="currentColor" strokeWidth={0} />,
  slack:     () => <Icon d={["M5 9.5h5V11.5a1.5 1.5 0 1 1-3 0", "M9.5 5v5H7.5a1.5 1.5 0 1 1 0-3", "M11 6.5h-5V4.5a1.5 1.5 0 1 1 3 0", "M6.5 11V6h2a1.5 1.5 0 1 1 0 3"]} />,
  git:       () => <Icon d={["M4 4v8", "M12 4v8", "M4 8h8"]} />,
  branch:    () => <Icon d={["M4 2v12", "M12 6v3a3 3 0 0 1-3 3H4", "M4 2.5v.01 M4 13.5v.01 M12 5.5v.01"]} />,
  filter:    () => <Icon d="M2.5 3.5h11l-4.5 5v4l-2 1V8.5l-4.5-5Z" />,
  more:      () => <Icon d="M4 8v.01 M8 8v.01 M12 8v.01" strokeWidth={2.4} />,
  send:      () => <Icon d="M14 2 7 9 M14 2 9.5 14l-2.5-5-5-2.5L14 2Z" />,
  paperclip: () => <Icon d="M12 6 6.5 11.5a2 2 0 1 1-3-3l6-6a3 3 0 1 1 4.5 4.5L7 13.5" />,
  bot:       () => <Icon d={["M5 5h6a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2Z", "M8 3.5v1.5", "M6 9v.01 M10 9v.01", "M2 9h1 M13 9h1"]} />,
  flask:     () => <Icon d="M6 2v4L2.5 12a2 2 0 0 0 1.7 3h7.6a2 2 0 0 0 1.7-3L10 6V2 M5.5 2h5" />,
  diff:      () => <Icon d="M4 3v10 M2 5h4 M2 11h4 M12 3v10 M10 8h4" />,
  pr:        () => <Icon d="M4.5 2.5v6 M4.5 8.5a2 2 0 1 0 0 4 2 2 0 0 0 0-4Z M11.5 11.5V5.5a2 2 0 0 0-2-2H8 M11.5 11.5a2 2 0 1 1 0 1 M9.5 1.5 8 3.5 9.5 5.5" />,
  flag:      () => <Icon d="M3 2v12 M3 3h7l-1 2 1 2H3" />,
  link:      () => <Icon d="M7 9.5 9.5 7 M6 5.5H4a2.5 2.5 0 0 0 0 5h2 M10 10.5h2a2.5 2.5 0 0 0 0-5h-2" />,
  bug:       () => <Icon d="M5 6V5a3 3 0 1 1 6 0v1 M3 9a5 5 0 1 0 10 0v-1H3v1Z M2 7l2 .5 M14 7l-2 .5 M2 12l2-1 M14 12l-2-1 M2 9.5h1 M13 9.5h1" />,
  globe:     () => <Icon d="M8 1.5a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13Z M1.5 8h13 M8 1.5a8 8 0 0 1 0 13 M8 1.5a8 8 0 0 0 0 13" />,
  expand:    () => <Icon d="M2 6V2h4 M14 6V2h-4 M2 10v4h4 M14 10v4h-4" />,
  settings:  () => <Icon d="M8 10a2 2 0 1 0 0-4 2 2 0 0 0 0 4Z M8 1.5v1 M8 13.5v1 M3.4 3.4l.7.7 M11.9 11.9l.7.7 M1.5 8h1 M13.5 8h1 M3.4 12.6l.7-.7 M11.9 4.1l.7-.7" />,
  video:     () => <Icon d={["M2 5h9a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H2a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1Z", "M12 7l3-2v6l-3-2"]} />,
  copy:      () => <Icon d="M5.5 5.5V3a1 1 0 0 1 1-1H13a1 1 0 0 1 1 1v6.5a1 1 0 0 1-1 1h-2.5 M3 5.5h7.5a1 1 0 0 1 1 1V13a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V6.5a1 1 0 0 1 1-1Z" />,
  plus:      () => <Icon d="M8 3v10 M3 8h10" />,
  trash:     () => <Icon d="M3 5h10 M5 5V3.5h6V5 M4 5l1 8.5h6L12 5" />,
  sparkle:   () => <Icon d="M8 1.5l1.4 5.1 5.1 1.4-5.1 1.4L8 14.5l-1.4-5.1-5.1-1.4 5.1-1.4z" fill="currentColor" strokeWidth={0} />,
  key:       () => <Icon d="M10.5 8a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z M7.5 8H14 M12 8v2 M14 8v1.5" />,
}
