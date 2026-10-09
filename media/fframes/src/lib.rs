//! seekfs motion graphics. Illustrated UI uses verified public-fixture results.
use fframes::{
    AudioMap, AudioTimestamp, AudioTrack, Color, Duration, FFramesContext, Frame, Scene, Scenes,
    Svgr, Transform, Video, animation::Easing, include_media_dir,
};
include_media_dir!(pub struct SeekfsLaunchMedia, "media");
pub const WIDTH: usize = 1920;
pub const HEIGHT: usize = 1080;
const FONT: &str = "DM Sans";
const MONO: &str = "JetBrains Mono";
const WHITE: &str = "#f0f4f6";
const DIM: &str = "#9baab4";
const GREEN: &str = "#40e8b5";
const BLUE: &str = "#5aa9ff";

#[derive(Debug, Clone, Copy)]
pub enum Kind {
    Intro,
    Desktop,
    Filters,
    Terminal,
    Json,
    Count,
    Outro,
}
#[derive(Debug)]
pub struct Shot {
    pub kind: Kind,
    pub seconds: f32,
    pub title: &'static str,
}
#[derive(Debug)]
pub struct SeekfsLaunchVideo {
    pub shots: Vec<Shot>,
    pub launch: bool,
}
impl SeekfsLaunchVideo {
    pub fn new(mode: &str) -> Self {
        let shots = match mode {
            "desktop" => vec![
                Shot {
                    kind: Kind::Desktop,
                    seconds: 8.,
                    title: "DesktopSearch",
                },
                Shot {
                    kind: Kind::Filters,
                    seconds: 8.,
                    title: "PathFilters",
                },
            ],
            "cli" => vec![
                Shot {
                    kind: Kind::Terminal,
                    seconds: 6.,
                    title: "TerminalSearch",
                },
                Shot {
                    kind: Kind::Json,
                    seconds: 6.,
                    title: "JsonOutput",
                },
                Shot {
                    kind: Kind::Count,
                    seconds: 6.,
                    title: "CountMatches",
                },
            ],
            _ => vec![
                Shot {
                    kind: Kind::Intro,
                    seconds: 5.,
                    title: "Hook",
                },
                Shot {
                    kind: Kind::Desktop,
                    seconds: 6.,
                    title: "DesktopSearch",
                },
                Shot {
                    kind: Kind::Filters,
                    seconds: 6.,
                    title: "PathFilters",
                },
                Shot {
                    kind: Kind::Terminal,
                    seconds: 6.,
                    title: "TerminalSearch",
                },
                Shot {
                    kind: Kind::Outro,
                    seconds: 7.,
                    title: "Launch",
                },
            ],
        };
        Self {
            shots,
            launch: mode != "desktop" && mode != "cli",
        }
    }
}
impl Video for SeekfsLaunchVideo {
    const FPS: usize = 30;
    const WIDTH: usize = WIDTH;
    const HEIGHT: usize = HEIGHT;
    const BACKGROUND_COLOR: Color = Color::BLACK;
    fn duration(&self) -> Duration<'_> {
        Duration::Auto
    }
    fn define_scenes(&self) -> Scenes<'_> {
        Scenes::from(
            self.shots
                .iter()
                .map(|s| s as &dyn Scene)
                .collect::<Vec<_>>(),
        )
    }
    fn audio(&self) -> AudioMap<'_> {
        if self.launch {
            AudioMap::from([AudioTrack::new(
                "launch.wav",
                AudioTimestamp::Second(0.)..AudioTimestamp::Eof,
            )
            .gain_db(-2.5)
            .fade_in(0.2)
            .fade_out(1.2)])
        } else {
            AudioMap::none()
        }
    }
    fn render_frame<'a>(&'a self, frame: Frame, ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        let clock = frame.global_index as f32 / Self::FPS as f32;
        let grid: Vec<_> = (0..15).map(|i|fframes::svgr!(<line x1={i*160} y1="0" x2={i*160} y2="1080" stroke="#132126" stroke-width="1" />)).collect();
        fframes::svgr!(<svg xmlns="http://www.w3.org/2000/svg" width={WIDTH} height={HEIGHT} viewBox="0 0 1920 1080">
            <defs><radialGradient id="ambient" cx="0.78" cy="0.42" r="0.7">
                <stop offset="0" stop-color="#0b3a35" stop-opacity="0.8" /><stop offset="1" stop-color="#080d12" stop-opacity="0" />
            </radialGradient></defs>
            <rect width="1920" height="1080" fill="#080d12" /><rect width="1920" height="1080" fill="url(#ambient)" />
            <g opacity="0.4">{grid}</g>
            <circle cx="1570" cy="510" r="465" fill="none" stroke="#173735" stroke-width="1" />
            <circle cx="1570" cy="510" r="360" fill="none" stroke="#173735" stroke-width="1" />
            <circle cx="1570" cy="510" r="255" fill="none" stroke="#173735" stroke-width="1" />
            <g transform={format!("rotate({} 1570 510)",clock*18.)} opacity="0.35">
                <line x1="1570" y1="510" x2="1900" y2="180" stroke={GREEN} stroke-width="2" />
            </g>
            {logo(ctx,140,74,70)}
            {text(228,122,40,"seekfs",WHITE,true)}
            <text x="1770" y="118" text-anchor="end" font-family={FONT} font-size="26" font-weight="500" fill={DIM}>"WINDOWS FILE SEARCH"</text>
            {ctx.render_scenes(&frame)}
            <line x1="150" y1="968" x2="1770" y2="968" stroke="#24443f" />
            {text(150,1018,28,"Local indexes. Your files. Your workflow.",DIM,false)}
            <text x="1770" y="1018" text-anchor="end" font-family={MONO} font-size="26" fill={GREEN}>"v1.11.0"</text>
        </svg>)
    }
}
fn type_on(text: &str, seconds: f32, start: f32, rate: f32) -> String {
    text.chars()
        .take(((seconds - start).max(0.) * rate) as usize)
        .collect()
}
fn text(x: i32, y: i32, size: i32, value: &str, fill: &str, mono: bool) -> Svgr<'static> {
    fframes::svgr!(<text x={x} y={y} font-family={if mono {MONO}else{FONT}} font-size={size} font-weight={if mono {400}else{500}} fill={fill.to_owned()}>{value.to_owned()}</text>)
}
fn logo<'a>(ctx: &FFramesContext<'a, '_>, x: i32, y: i32, size: i32) -> Svgr<'a> {
    ctx.get_image("logo.png")
        .map(|i| fframes::svgr!(<image href={i.href()} x={x} y={y} width={size} height={size} />))
        .unwrap_or_default()
}
fn product_ui<'a>(
    ctx: &FFramesContext<'a, '_>,
    query: &str,
    filtered: bool,
    ready: bool,
) -> Svgr<'a> {
    let items = if filtered {
        vec![("main.go", "cmd/indexer"), ("main.go", "cmd/seekfs")]
    } else {
        vec![
            ("main.go", "cmd/indexer"),
            ("main.go", "cmd/seekfs"),
            ("main-guide.md", "docs"),
        ]
    };
    let rows:Vec<_> = items.iter().enumerate().map(|(i,(name,path))|{
        let y=298+i as i32*84;
        fframes::svgr!(<g>
            <rect x="25" y={y-47} width="860" height="75" rx="7" fill={if i==0 {"#11253a"}else{"#0d1116"}} />
            <rect x="46" y={y-25} width="20" height="25" rx="3" fill="none" stroke={BLUE} stroke-width="2" />
            {text(85,y,26,name,WHITE,true)}{text(345,y,24,path,DIM,true)}{text(665,y,24,"14 B",DIM,true)}{text(762,y,22,"Oct 08",DIM,false)}
        </g>)
    }).collect();
    fframes::svgr!(<g>
        <rect x="5" y="5" width="910" height="623" rx="23" fill="#051013" opacity="0.4" />
        <rect width="910" height="620" rx="20" fill="#0b0b0e" stroke="#314149" stroke-width="2" />
        {logo(ctx,30,24,36)}{text(80,51,26,"seekfs",WHITE,true)}
        <circle cx="841" cy="38" r="4" fill="#5c6670" /><circle cx="866" cy="38" r="4" fill="#5c6670" />
        {text(33,128,32,">",BLUE,true)}{text(73,128,29,query,WHITE,true)}
        <line x1="25" y1="153" x2="885" y2="153" stroke="#2a2c33" />
        {text(42,210,23,"Name",DIM,false)}{text(345,210,23,"Path",DIM,false)}{text(665,210,23,"Size",DIM,false)}{text(759,210,22,"Modified",DIM,false)}
        <line x1="25" y1="228" x2="885" y2="228" stroke="#20242a" />
        <g opacity={if ready {1.}else{0.}}>{rows}</g>
        {text(32,586,23,if ready {if filtered {"2 matching files"}else{"3 matching files"}}else{"Type to search"},DIM,false)}
        <circle cx="863" cy="578" r="6" fill={GREEN} />
    </g>)
}
impl Scene for Shot {
    fn name(&self) -> &'static str {
        self.title
    }
    fn duration(&self) -> Duration<'_> {
        Duration::Seconds(self.seconds)
    }
    fn render_frame<'a>(&'a self, frame: Frame, ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        let t = frame.seconds();
        let content = match self.kind {
            Kind::Intro => fframes::svgr!(<g>
                {text(150,260,30,"MEET SEEKFS",GREEN,true)}{text(150,425,122,"Find your files.",WHITE,false)}{text(150,575,122,"Keep your flow.",WHITE,false)}
                {text(155,695,43,"A desktop prompt. A powerful CLI.",DIM,false)}
                <g transform={frame.animate(fframes::timeline!(at 0., animate Transform::translate(70,0)=>Transform::translate(0,0), Easing::Spring {mass:1.,stiffness:150.,damping:20.},))}>
                    <circle cx="1490" cy="535" r="220" fill="none" stroke="#344249" stroke-width="48" />
                    <circle cx="1490" cy="535" r="68" fill="#344249" />
                    <g transform={format!("rotate({} 1490 535)",t*54.)}>
                        <path d="M1490 315 A220 220 0 0 1 1684 639" fill="none" stroke={GREEN} stroke-width="48" />
                        <path d="M1490 535 L1645 367 L1672 445 Z" fill="#12ddcf" />
                    </g>
                    <g transform={frame.animate(fframes::timeline!(at 0.3,animate Transform::translate(0,60)=>Transform::translate(0,0),Easing::Spring {mass:1.,stiffness:130.,damping:18.},))}>
                        <rect x="1260" y="245" width="130" height="58" rx="14" fill="#123c34" stroke="#32806d" />{text(1287,285,28,".go",GREEN,true)}
                        <rect x="1655" y="727" width="130" height="58" rx="14" fill="#12303c" stroke="#326080" />{text(1680,767,28,".md",BLUE,true)}
                    </g>
                </g>
                <rect x="155" y="755" width={frame.animate(fframes::timeline!(at 0.1=>1.3, animate 0.5_f32=>280.0, Easing::EaseOut,))} height="6" rx="3" fill={GREEN} />
            </g>),
            Kind::Desktop | Kind::Filters => {
                let filtered = matches!(self.kind, Kind::Filters);
                let typed = type_on(
                    if filtered {
                        "ext:go dir:cmd main"
                    } else {
                        "main"
                    },
                    t,
                    0.8,
                    if filtered { 18. } else { 5. },
                );
                fframes::svgr!(<g>
                    {text(150,275,30,if filtered {"SEARCH WITH INTENT"}else{"DESKTOP SEARCH"},GREEN,true)}
                    {text(150,405,94,if filtered {"Narrow"}else{"A prompt."},WHITE,false)}{text(150,520,94,if filtered {"the noise."}else{"Your files."},WHITE,false)}
                    {text(155,635,38,if filtered {"Filename. Path. Extension."}else{"Type a name. Pick a result."},DIM,false)}
                    {text(155,697,38,if filtered {"Combine filters in one query."}else{"Stay in your flow."},DIM,false)}
                    <g transform={Transform::translate(860,245)}>{product_ui(ctx,&typed,filtered,t>if filtered {2.0}else{1.7})}</g>
                </g>)
            }
            Kind::Terminal | Kind::Json | Kind::Count => {
                let json = matches!(self.kind, Kind::Json);
                let count = matches!(self.kind, Kind::Count);
                let command = if json {
                    "seekfs search --json main"
                } else if count {
                    "seekfs count main"
                } else {
                    "seekfs search -path \"ext:go dir:cmd main\""
                };
                let typed = type_on(command, t, 0.55, 30.);
                let output: Vec<_> = if t > 2.2 {
                    if json {
                        vec![
                            text(55, 190, 31, "{", GREEN, true),
                            text(85, 240, 30, "\"ok\": true,", WHITE, true),
                            text(85, 290, 30, "\"query\": \"main\",", WHITE, true),
                            text(85, 340, 30, "\"count\": 3,", WHITE, true),
                            text(85, 390, 30, "\"complete\": true,", WHITE, true),
                            text(85, 440, 30, "\"results\": [ ... ]", WHITE, true),
                            text(55, 490, 31, "}", GREEN, true),
                        ]
                    } else if count {
                        vec![
                            text(55, 230, 95, "3", GREEN, true),
                            text(55, 330, 29, "matching files", DIM, true),
                        ]
                    } else {
                        vec![
                            text(55, 230, 29, "cmd/indexer/main.go", WHITE, true),
                            text(55, 292, 29, "cmd/seekfs/main.go", WHITE, true),
                        ]
                    }
                } else {
                    vec![]
                };
                fframes::svgr!(<g>
                    {text(150,265,28,if json {"AGENT-FRIENDLY OUTPUT"}else if count {"COUNT WITHOUT A RESULT LIST"}else{"RESIDENT SERVICE + CLI"},GREEN,true)}
                    {text(150,407,90,if json {"Ready for"}else if count {"Just the"}else{"Built for your"},WHITE,false)}{text(150,515,90,if json {"scripts."}else if count {"number."}else{"terminal, too."},WHITE,false)}
                    {text(155,638,34,if json {"Structured JSON for automation."}else if count {"Use seekfs count in your workflow."}else{"Search. Count. Filter. JSON."},DIM,false)}
                    {text(155,700,34,"For developers and agents.",DIM,false)}
                    <g transform={Transform::translate(850,260)}>
                        <rect width="925" height="570" rx="20" fill="#0b1016" stroke="#32414e" stroke-width="2" />
                        <circle cx="37" cy="35" r="6" fill="#f38a85" /><circle cx="62" cy="35" r="6" fill="#edcc80" /><circle cx="87" cy="35" r="6" fill="#75c8a4" />
                        {text(118,44,24,"PowerShell",DIM,true)}<line x1="25" y1="65" x2="900" y2="65" stroke="#25313c" />
                        {text(40,128,28,">",GREEN,true)}{text(73,128,27,&typed,WHITE,true)}{output}
                    </g>
                </g>)
            }
            Kind::Outro => fframes::svgr!(<g>
                {logo(ctx,145,245,185)}{text(365,371,116,"seekfs",WHITE,true)}
                {text(150,535,91,"Windows file search.",WHITE,false)}{text(150,645,91,"Open source.",GREEN,false)}
                <rect x="155" y="723" width="670" height="90" rx="15" fill={GREEN} />{text(192,782,40,"Download v1.11.0 on GitHub","#061511",false)}
                {text(155,889,36,"github.com/holdfast-labs/seekfs",DIM,true)}
                <g opacity="0.75" transform={frame.animate(fframes::timeline!(at 0., animate Transform::translate(55,0)=>Transform::translate(0,0), Easing::Spring {mass:1.,stiffness:120.,damping:20.},))}>
                    <circle cx="1450" cy="570" r="265" fill="none" stroke={GREEN} stroke-width="3" /><circle cx="1450" cy="570" r="170" fill="none" stroke={GREEN} stroke-width="2" /><circle cx="1450" cy="570" r="65" fill={GREEN} opacity="0.22" />
                    <line x1="1185" y1="570" x2="1715" y2="570" stroke={GREEN} stroke-width="1" /><line x1="1450" y1="305" x2="1450" y2="835" stroke={GREEN} stroke-width="1" />
                </g>
            </g>),
        };
        let opacity = if matches!(self.kind, Kind::Intro) {
            1.
        } else {
            frame.animate(&*ENTER)
        };
        fframes::svgr!(<g opacity={opacity}>
            <g transform={frame.animate(fframes::timeline!(at 0., animate Transform::translate(0,28)=>Transform::translate(0,0), Easing::Spring {mass:1.,stiffness:200.,damping:25.},))}>{content}</g>
        </g>)
    }
}
static ENTER: std::sync::LazyLock<fframes::animation::KeyFramesAnimation<f32>> =
    std::sync::LazyLock::new(
        || fframes::timeline!(at 0.0=>0.35,animate 0.0_f32=>1.0,Easing::EaseOut,),
    );
